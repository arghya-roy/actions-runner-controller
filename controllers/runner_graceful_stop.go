package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/actions-runner-controller/actions-runner-controller/github"
	"github.com/go-logr/logr"
	gogithub "github.com/google/go-github/v39/github"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	unregistrationCompleteTimestamp = "unregistration-complete-timestamp"
	unregistrationStartTimestamp    = "unregistration-start-timestamp"

	// DefaultUnregistrationTimeout is the duration until ARC gives up retrying the combo of ListRunners API (to detect the runner ID by name)
	// and RemoveRunner API (to actually unregister the runner) calls.
	// This needs to be longer than 60 seconds because a part of the combo, the ListRunners API, seems to use the Cache-Control header of max-age=60s
	// and that instructs our cache library httpcache to cache responses for 60 seconds, which results in ARC unable to see the runner in the ListRunners response
	// up to 60 seconds (or even more depending on the situation).
	DefaultUnregistrationTimeout = 60 * time.Second

	// This can be any value but a larger value can make an unregistration timeout longer than configured in practice.
	DefaultUnregistrationRetryDelay = 30 * time.Second
)

// tickRunnerGracefulStop reconciles the runner and the runner pod in a way so that
// we can delete the runner pod without disrupting a workflow job.
//
// This function returns a non-nil pointer to corev1.Pod as the first return value
// if the runner is considered to have gracefully stopped, hence it's pod is safe for deletion.
//
// It's a "tick" operation so a graceful stop can take multiple calls to complete.
// This function is designed to complete a length graceful stop process in a unblocking way.
// When it wants to be retried later, the function returns a non-nil *ctrl.Result as the second return value, may or may not populating the error in the second return value.
// The caller is expected to return the returned ctrl.Result and error to postpone the current reconcilation loop and trigger a scheduled retry.
func tickRunnerGracefulStop(ctx context.Context, unregistrationTimeout time.Duration, retryDelay time.Duration, log logr.Logger, ghClient *github.Client, c client.Client, enterprise, organization, repository, runner string, pod *corev1.Pod) (*corev1.Pod, *ctrl.Result, error) {
	if pod != nil {
		if _, ok := getAnnotation(pod, unregistrationStartTimestamp); !ok {
			updated := pod.DeepCopy()
			setAnnotation(updated, unregistrationStartTimestamp, time.Now().Format(time.RFC3339))
			if err := c.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
				log.Error(err, fmt.Sprintf("Failed to patch pod to have %s annotation", unregistrationStartTimestamp))
				return nil, &ctrl.Result{}, err
			}
			pod = updated

			log.Info("Runner has started unregistration")
		} else {
			log.Info("Runner has already started unregistration")
		}
	}

	if res, err := ensureRunnerUnregistration(ctx, unregistrationTimeout, retryDelay, log, ghClient, enterprise, organization, repository, runner, pod); res != nil {
		return nil, res, err
	}

	if pod != nil {
		if _, ok := getAnnotation(pod, unregistrationCompleteTimestamp); !ok {
			updated := pod.DeepCopy()
			setAnnotation(updated, unregistrationCompleteTimestamp, time.Now().Format(time.RFC3339))
			if err := c.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
				log.Error(err, fmt.Sprintf("Failed to patch pod to have %s annotation", unregistrationCompleteTimestamp))
				return nil, &ctrl.Result{}, err
			}
			pod = updated

			log.Info("Runner has completed unregistration")
		} else {
			log.Info("Runner has already completed unregistration")
		}
	}

	return pod, nil, nil
}

// If the first return value is nil, it's safe to delete the runner pod.
func ensureRunnerUnregistration(ctx context.Context, unregistrationTimeout time.Duration, retryDelay time.Duration, log logr.Logger, ghClient *github.Client, enterprise, organization, repository, runner string, pod *corev1.Pod) (*ctrl.Result, error) {
	ok, err := unregisterRunner(ctx, ghClient, enterprise, organization, repository, runner)
	if err != nil {
		if errors.Is(err, &gogithub.RateLimitError{}) {
			// We log the underlying error when we failed calling GitHub API to list or unregisters,
			// or the runner is still busy.
			log.Error(
				err,
				fmt.Sprintf(
					"Failed to unregister runner due to GitHub API rate limits. Delaying retry for %s to avoid excessive GitHub API calls",
					retryDelayOnGitHubAPIRateLimitError,
				),
			)

			return &ctrl.Result{RequeueAfter: retryDelayOnGitHubAPIRateLimitError}, err
		}

		log.Error(err, "Failed to unregister runner before deleting the pod.")

		return &ctrl.Result{}, err
	} else if ok {
		log.Info("Runner has just been unregistered. Removing the runner pod.")
	} else if pod == nil {
		// `r.unregisterRunner()` will returns `false, nil` if the runner is not found on GitHub.
		// However, that doesn't always mean the pod can be safely removed.
		//
		// If the pod does not exist for the runner,
		// it may be due to that the runner pod has never been created.
		// In that case we can safely assume that the runner will never be registered.

		log.Info("Runner was not found on GitHub and the runner pod was not found on Kuberntes.")
	} else if pod.Annotations[unregistrationCompleteTimestamp] != "" {
		// If it's already unregistered in the previous reconcilation loop,
		// you can safely assume that it won't get registered again so it's safe to delete the runner pod.
		log.Info("Runner pod is marked as already unregistered.")
	} else if runnerPodOrContainerIsStopped(pod) {
		// If it's an ephemeral runner with the actions/runner container exited with 0,
		// we can safely assume that it has unregistered itself from GitHub Actions
		// so it's natural that RemoveRunner fails due to 404.

		// If pod has ended up succeeded we need to restart it
		// Happens e.g. when dind is in runner and run completes
		log.Info("Runner pod has been stopped with a successful status.")
	} else if ts := pod.Annotations[unregistrationStartTimestamp]; ts != "" {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return &ctrl.Result{RequeueAfter: retryDelay}, err
		}

		if r := time.Until(t.Add(unregistrationTimeout)); r > 0 {
			log.Info("Runner unregistration is in-progress.", "timeout", unregistrationTimeout, "remaining", r)
			return &ctrl.Result{RequeueAfter: retryDelay}, err
		}

		log.Info("Runner unregistration has been timed out. The runner pod will be deleted soon.", "timeout", unregistrationTimeout)
	} else {
		// A runner and a runner pod that is created by this version of ARC should match
		// any of the above branches.
		//
		// But we leave this match all branch for potential backward-compatibility.
		// The caller is expected to take appropriate actions, like annotating the pod as started the unregistration process,
		// and retry later.
		log.V(1).Info("Runner unregistration is being retried later.")

		return &ctrl.Result{RequeueAfter: retryDelay}, nil
	}

	return nil, nil
}

func getAnnotation(pod *corev1.Pod, key string) (string, bool) {
	if pod.Annotations == nil {
		return "", false
	}

	v, ok := pod.Annotations[key]

	return v, ok
}

func setAnnotation(pod *corev1.Pod, key, value string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[key] = value
}

// unregisterRunner unregisters the runner from GitHub Actions by name.
//
// This function returns:
//
// Case 1. (true, nil) when it has successfully unregistered the runner.
// Case 2. (false, nil) when (2-1.) the runner has been already unregistered OR (2-2.) the runner will never be created OR (2-3.) the runner is not created yet and it is about to be registered(hence we couldn't see it's existence from GitHub Actions API yet)
// Case 3. (false, err) when it postponed unregistration due to the runner being busy, or it tried to unregister the runner but failed due to
//   an error returned by GitHub API.
//
// When the returned values is "Case 2. (false, nil)", the caller must handle the three possible sub-cases appropriately.
// In other words, all those three sub-cases cannot be distinguished by this function alone.
//
// - Case "2-1." can happen when e.g. ARC has successfully unregistered in a previous reconcilation loop or it was an ephemeral runner that finished it's job run(an ephemeral runner is designed to stop after a job run).
//   You'd need to maintain the runner state(i.e. if it's already unregistered or not) somewhere,
//   so that you can either not call this function at all if the runner state says it's already unregistered, or determine that it's case "2-1." when you got (false, nil).
//
// - Case "2-2." can happen when e.g. the runner registration token was somehow broken so that `config.sh` within the runner container was never meant to succeed.
//   Waiting and retrying forever on this case is not a solution, because `config.sh` won't succeed with a wrong token hence the runner gets stuck in this state forever.
//   There isn't a perfect solution to this, but a practical workaround would be implement a "grace period" in the caller side.
//
// - Case "2-3." can happen when e.g. ARC recreated an ephemral runner pod in a previous reconcilation loop and then it was requested to delete the runner before the runner comes up.
//   If handled inappropriately, this can cause a race condition betweeen a deletion of the runner pod and GitHub scheduling a workflow job onto the runner.
//
// Once successfully detected case "2-1." or "2-2.", you can safely delete the runner pod because you know that the runner won't come back
// as long as you recreate the runner pod.
//
// If it was "2-3.", you need a workaround to avoid the race condition.
//
// You shall introduce a "grace period" mechanism, similar or equal to that is required for "Case 2-2.", so that you ever
// start the runner pod deletion only after it's more and more likely that the runner pod is not coming up.
//
// Beware though, you need extra care to set an appropriate grace period depending on your environment.
// There isn't a single right grace period that works for everyone.
// The longer the grace period is, the earlier a cluster resource shortage can occur due to throttoled runner pod deletions,
// while the shorter the grace period is, the more likely you may encounter the race issue.
func unregisterRunner(ctx context.Context, client *github.Client, enterprise, org, repo, name string) (bool, error) {
	runners, err := client.ListRunners(ctx, enterprise, org, repo)
	if err != nil {
		return false, err
	}

	id := int64(0)
	for _, runner := range runners {
		if runner.GetName() == name {
			id = runner.GetID()
			break
		}
	}

	if id == int64(0) {
		return false, nil
	}

	// For the record, historically ARC did not try to call RemoveRunner on a busy runner, but it's no longer true.
	// The reason ARC did so was to let a runner running a job to not stop prematurely.
	//
	// However, we learned that RemoveRunner already has an ability to prevent stopping a busy runner,
	// so ARC doesn't need to do anything special for a graceful runner stop.
	// It can just call RemoveRunner, and if it returned 200 you're guaranteed that the runner will not automatically come back and
	// the runner pod is safe for deletion.
	//
	// Trying to remove a busy runner can result in errors like the following:
	//    failed to remove runner: DELETE https://api.github.com/repos/actions-runner-controller/mumoshu-actions-test/actions/runners/47: 422 Bad request - Runner \"example-runnerset-0\" is still running a job\" []
	//
	// # NOTES
	//
	// - It can be "status=offline" at the same time but that's another story.
	// - After https://github.com/actions-runner-controller/actions-runner-controller/pull/1127, ListRunners responses that are used to
	//   determine if the runner is busy can be more outdated than before, as those responeses are now cached for 60 seconds.
	// - Note that 60 seconds is controlled by the Cache-Control response header provided by GitHub so we don't have a strict control on it but we assume it won't
	//   change from 60 seconds.
	//
	// TODO: Probably we can just remove the runner by ID without seeing if the runner is busy, by treating it as busy when a remove-runner call failed with 422?
	if err := client.RemoveRunner(ctx, enterprise, org, repo, id); err != nil {
		return false, err
	}

	return true, nil
}
