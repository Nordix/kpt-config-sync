// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parse

import (
	"context"
	"os"
	"time"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"kpt.dev/configsync/pkg/declared"
	"kpt.dev/configsync/pkg/hydrate"
	"kpt.dev/configsync/pkg/importer/filesystem/cmpath"
	"kpt.dev/configsync/pkg/metrics"
	"kpt.dev/configsync/pkg/status"
	webhookconfiguration "kpt.dev/configsync/pkg/webhook/configuration"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	triggerResync             = "resync"
	triggerReimport           = "reimport"
	triggerRetry              = "retry"
	triggerManagementConflict = "managementConflict"
	triggerWatchUpdate        = "watchUpdate"
)

const (
	// RenderingInProgress means that the configs are still being rendered by Config Sync.
	RenderingInProgress string = "Rendering is still in progress"

	// RenderingSucceeded means that the configs have been rendered successfully.
	RenderingSucceeded string = "Rendering succeeded"

	// RenderingFailed means that the configs have failed to be rendered.
	RenderingFailed string = "Rendering failed"

	// RenderingSkipped means that the configs don't need to be rendered.
	RenderingSkipped string = "Rendering skipped"
)

// Run keeps checking whether a parse-apply-watch loop is necessary and starts a loop if needed.
func Run(ctx context.Context, p Parser) {
	opts := p.options()
	// Use timers, not tickers.
	// Tickers can cause memory leaks and continuous execution, when execution
	// takes longer than the tick duration.
	runTimer := time.NewTimer(opts.pollingPeriod)
	defer runTimer.Stop()

	resyncTimer := time.NewTimer(opts.resyncPeriod)
	defer resyncTimer.Stop()

	retryTimer := time.NewTimer(opts.retryPeriod)
	defer retryTimer.Stop()

	statusUpdateTimer := time.NewTimer(opts.statusUpdatePeriod)
	defer statusUpdateTimer.Stop()

	state := &reconcilerState{}
	for {
		select {
		case <-ctx.Done():
			return

		// Re-apply even if no changes have been detected.
		// This case should be checked first since it resets the cache.
		case <-resyncTimer.C:
			klog.Infof("It is time for a force-resync")
			// Reset the cache to make sure all the steps of a parse-apply-watch loop will run.
			// The cached sourceState will not be reset to avoid reading all the source files unnecessarily.
			state.resetAllButSourceState()
			run(ctx, p, triggerResync, state)

			resyncTimer.Reset(opts.resyncPeriod)             // Schedule resync attempt
			retryTimer.Reset(opts.retryPeriod)               // Schedule retry attempt
			statusUpdateTimer.Reset(opts.statusUpdatePeriod) // Schedule status update attempt

		// Re-import declared resources from the filesystem (from git-sync).
		case <-runTimer.C:
			run(ctx, p, triggerReimport, state)

			runTimer.Reset(opts.pollingPeriod)               // Schedule re-run attempt
			retryTimer.Reset(opts.retryPeriod)               // Schedule retry attempt
			statusUpdateTimer.Reset(opts.statusUpdatePeriod) // Schedule status update attempt

		// Retry if there was an error, conflict, or any watches need to be updated.
		case <-retryTimer.C:
			var trigger string
			if opts.managementConflict() {
				// Reset the cache to make sure all the steps of a parse-apply-watch loop will run.
				// The cached sourceState will not be reset to avoid reading all the source files unnecessarily.
				state.resetAllButSourceState()
				trigger = triggerManagementConflict
				// When conflict is detected, wait longer (same as the polling frequency) for the next retry.
				time.Sleep(opts.pollingPeriod)
			} else if state.cache.needToRetry && state.cache.readyToRetry() {
				klog.Infof("The last reconciliation failed")
				trigger = triggerRetry
			} else if opts.needToUpdateWatch() {
				klog.Infof("Some watches need to be updated")
				trigger = triggerWatchUpdate
			} else {
				// Don't reset the retry timer if there's nothing to retry.
				continue
			}
			run(ctx, p, trigger, state)

			retryTimer.Reset(opts.retryPeriod)               // Schedule retry attempt
			statusUpdateTimer.Reset(opts.statusUpdatePeriod) // Schedule status update attempt

		// Update the sync status to report management conflicts (from the remediator).
		case <-statusUpdateTimer.C:
			// Skip sync status update if the .status.sync.commit is out of date.
			// This avoids overwriting a newer Syncing condition with the status
			// from an older commit.
			if state.syncStatus.commit == state.sourceStatus.commit &&
				state.syncStatus.commit == state.renderingStatus.commit {

				klog.V(3).Info("Updating sync status (periodic while not syncing)")
				if err := setSyncStatus(ctx, p, state, p.Syncing(), p.SyncErrors()); err != nil {
					klog.Warningf("failed to update sync status: %v", err)
				}
			}

			statusUpdateTimer.Reset(opts.statusUpdatePeriod) // Schedule status update attempt
		}
	}
}

func run(ctx context.Context, p Parser, trigger string, state *reconcilerState) {
	var syncDir cmpath.Absolute
	gs := sourceStatus{}
	gs.commit, syncDir, gs.errs = hydrate.SourceCommitAndDir(p.options().SourceType, p.options().SourceDir, p.options().SyncDir, p.options().reconcilerName)

	// If failed to fetch the source commit and directory, set `.status.source` to fail early.
	// Otherwise, set `.status.rendering` before `.status.source` because the parser needs to
	// read and parse the configs after rendering is done and there might have errors.
	if gs.errs != nil {
		gs.lastUpdate = metav1.Now()
		var setSourceStatusErr error
		if state.needToSetSourceStatus(gs) {
			klog.V(3).Info("Updating source status (before read): %#v", gs)
			setSourceStatusErr = p.setSourceStatus(ctx, gs)
			if setSourceStatusErr == nil {
				state.sourceStatus = gs
				state.syncingConditionLastUpdate = gs.lastUpdate
			}
		}
		state.invalidate(status.Append(gs.errs, setSourceStatusErr))
		return
	}

	rs := renderingStatus{
		commit: gs.commit,
	}

	// set the rendering status by checking the done file.
	doneFilePath := p.options().RepoRoot.Join(cmpath.RelativeSlash(hydrate.DoneFile)).OSPath()
	_, err := os.Stat(doneFilePath)
	if os.IsNotExist(err) || (err == nil && hydrate.DoneCommit(doneFilePath) != gs.commit) {
		rs.message = RenderingInProgress
		rs.lastUpdate = metav1.Now()
		klog.V(3).Info("Updating rendering status (before read): %#v", rs)
		setRenderingStatusErr := p.setRenderingStatus(ctx, state.renderingStatus, rs)
		if setRenderingStatusErr == nil {
			state.reset()
			state.renderingStatus = rs
			state.syncingConditionLastUpdate = rs.lastUpdate
		} else {
			var m status.MultiError
			state.invalidate(status.Append(m, setRenderingStatusErr))
		}
		return
	}
	if err != nil {
		rs.message = RenderingFailed
		rs.lastUpdate = metav1.Now()
		rs.errs = status.InternalHydrationError(err, "unable to read the done file: %s", doneFilePath)
		klog.V(3).Info("Updating rendering status (before read): %#v", rs)
		setRenderingStatusErr := p.setRenderingStatus(ctx, state.renderingStatus, rs)
		if setRenderingStatusErr == nil {
			state.renderingStatus = rs
			state.syncingConditionLastUpdate = rs.lastUpdate
		}
		state.invalidate(status.Append(rs.errs, setRenderingStatusErr))
		return
	}

	// rendering is done, starts to read the source or hydrated configs.
	oldSyncDir := state.cache.source.syncDir
	// `read` is called no matter what the trigger is.
	ps := sourceState{
		commit:  gs.commit,
		syncDir: syncDir,
	}
	if errs := read(ctx, p, trigger, state, ps); errs != nil {
		state.invalidate(errs)
		return
	}

	newSyncDir := state.cache.source.syncDir
	// The parse-apply-watch sequence will be skipped if the trigger type is `triggerReimport` and
	// there is no new source changes. The reasons are:
	//   * If a former parse-apply-watch sequence for syncDir succeeded, there is no need to run the sequence again;
	//   * If all the former parse-apply-watch sequences for syncDir failed, the next retry will call the sequence;
	//   * The retry logic tracks the number of reconciliation attempts failed with the same errors, and when
	//     the next retry should happen. Calling the parse-apply-watch sequence here makes the retry logic meaningless.
	if trigger == triggerReimport && oldSyncDir == newSyncDir {
		return
	}

	errs := parseAndUpdate(ctx, p, trigger, state)
	if errs != nil {
		state.invalidate(errs)
		return
	}

	// Only checkpoint the state after *everything* succeeded, including status update.
	state.checkpoint()
}

// read reads config files from source if no rendering is needed, or from hydrated output if rendering is done.
// It also updates the .status.rendering and .status.source fields.
func read(ctx context.Context, p Parser, trigger string, state *reconcilerState, sourceState sourceState) status.MultiError {
	hydrationStatus, sourceStatus := readFromSource(ctx, p, trigger, state, sourceState)
	// Return the transient errors here to avoid surfacing them to the R*Sync status field.
	// The transient errors might be auto-resolved in the next retry loop, so no need to expose to users.
	if status.HasTransientErrors(hydrationStatus.errs) {
		return hydrationStatus.errs
	}
	if status.HasTransientErrors(sourceStatus.errs) {
		return sourceStatus.errs
	}
	hydrationStatus.lastUpdate = metav1.Now()
	// update the rendering status before source status because the parser needs to
	// read and parse the configs after rendering is done and there might have errors.
	klog.V(3).Info("Updating rendering status (after read): %#v", hydrationStatus)
	setRenderingStatusErr := p.setRenderingStatus(ctx, state.renderingStatus, hydrationStatus)
	if setRenderingStatusErr == nil {
		state.renderingStatus = hydrationStatus
		state.syncingConditionLastUpdate = hydrationStatus.lastUpdate
	}
	renderingErrs := status.Append(hydrationStatus.errs, setRenderingStatusErr)
	if renderingErrs != nil {
		return renderingErrs
	}

	if sourceStatus.errs == nil {
		return nil
	}

	// Only call `setSourceStatus` if `readFromSource` fails.
	// If `readFromSource` succeeds, `parse` may still fail.
	sourceStatus.lastUpdate = metav1.Now()
	var setSourceStatusErr error
	if state.needToSetSourceStatus(sourceStatus) {
		klog.V(3).Info("Updating source status (after read): %#v", sourceStatus)
		setSourceStatusErr := p.setSourceStatus(ctx, sourceStatus)
		if setSourceStatusErr == nil {
			state.sourceStatus = sourceStatus
			state.syncingConditionLastUpdate = sourceStatus.lastUpdate
		}
	}

	return status.Append(sourceStatus.errs, setSourceStatusErr)
}

// readFromSource reads the source or hydrated configs, checks whether the sourceState in
// the cache is up-to-date. If the cache is not up-to-date, reads all the source or hydrated files.
// readFromSource returns the rendering status and source status.
func readFromSource(ctx context.Context, p Parser, trigger string, state *reconcilerState, sourceState sourceState) (renderingStatus, sourceStatus) {
	opts := p.options()
	start := time.Now()

	hydrationStatus := renderingStatus{
		commit: sourceState.commit,
	}
	sourceStatus := sourceStatus{
		commit: sourceState.commit,
	}

	// Check if the hydratedRoot directory exists.
	// If exists, read the hydrated directory. Otherwise, read the source directory.
	absHydratedRoot, err := cmpath.AbsoluteOS(opts.HydratedRoot)
	if err != nil {
		hydrationStatus.message = RenderingFailed
		hydrationStatus.errs = status.InternalHydrationError(err, "hydrated-dir must be an absolute path")
		return hydrationStatus, sourceStatus
	}

	var hydrationErr hydrate.HydrationError
	if _, err := os.Stat(absHydratedRoot.OSPath()); err == nil {
		sourceState, hydrationErr = opts.readHydratedDir(absHydratedRoot, opts.HydratedLink, opts.reconcilerName)
		if hydrationErr != nil {
			hydrationStatus.message = RenderingFailed
			hydrationStatus.errs = status.HydrationError(hydrationErr.Code(), hydrationErr)
			return hydrationStatus, sourceStatus
		}
		hydrationStatus.message = RenderingSucceeded
	} else if !os.IsNotExist(err) {
		hydrationStatus.message = RenderingFailed
		hydrationStatus.errs = status.InternalHydrationError(err, "unable to evaluate the hydrated path %s", absHydratedRoot.OSPath())
		return hydrationStatus, sourceStatus
	} else {
		hydrationStatus.message = RenderingSkipped
	}

	if sourceState.syncDir == state.cache.source.syncDir {
		return hydrationStatus, sourceStatus
	}

	klog.Infof("New source changes (%s) detected, reset the cache", sourceState.syncDir.OSPath())

	// Reset the cache to make sure all the steps of a parse-apply-watch loop will run.
	state.resetCache()

	// Read all the files under state.syncDir
	sourceStatus.errs = opts.readConfigFiles(&sourceState, p)
	if sourceStatus.errs == nil {
		// Set `state.cache.source` after `readConfigFiles` succeeded
		state.cache.source = sourceState
	}
	metrics.RecordParserDuration(ctx, trigger, "read", metrics.StatusTagKey(sourceStatus.errs), start)
	return hydrationStatus, sourceStatus
}

func parseSource(ctx context.Context, p Parser, trigger string, state *reconcilerState) status.MultiError {
	if state.cache.parserResultUpToDate() {
		return nil
	}

	start := time.Now()
	objs, sourceErrs := p.parseSource(ctx, state.cache.source)
	metrics.RecordParserDuration(ctx, trigger, "parse", metrics.StatusTagKey(sourceErrs), start)
	state.cache.setParserResult(objs, sourceErrs)

	if !status.HasBlockingErrors(sourceErrs) {
		err := webhookconfiguration.Update(ctx, p.options().k8sClient(), p.options().discoveryClient(), objs)
		if err != nil {
			// Don't block if updating the admission webhook fails.
			// Return an error instead if we remove the remediator as otherwise we
			// will simply never correct the type.
			// This should be treated as a warning once we have
			// that capability.
			klog.Errorf("Failed to update admission webhook: %v", err)
			// TODO: Handle case where multiple reconciler Pods try to
			//  create or update the Configuration simultaneously.
		}
	}

	return sourceErrs
}

func parseAndUpdate(ctx context.Context, p Parser, trigger string, state *reconcilerState) status.MultiError {
	klog.V(3).Info("Parser starting...")
	sourceErrs := parseSource(ctx, p, trigger, state)
	klog.V(3).Info("Parser stopped")
	newSourceStatus := sourceStatus{
		commit:     state.cache.source.commit,
		errs:       sourceErrs,
		lastUpdate: metav1.Now(),
	}
	if state.needToSetSourceStatus(newSourceStatus) {
		klog.V(3).Info("Updating source status (after parse): %#v", newSourceStatus)
		if err := p.setSourceStatus(ctx, newSourceStatus); err != nil {
			// If `p.setSourceStatus` fails, we terminate the reconciliation.
			// If we call `update` in this case and `update` succeeds, `Status.Source.Commit` would end up be older
			// than `Status.Sync.Commit`.
			return status.Append(sourceErrs, err)
		}
		state.sourceStatus = newSourceStatus
		state.syncingConditionLastUpdate = newSourceStatus.lastUpdate
	}

	if status.HasBlockingErrors(sourceErrs) {
		return sourceErrs
	}

	// Create a new context with its cancellation function.
	ctxForUpdateSyncStatus, cancel := context.WithCancel(context.Background())

	go updateSyncStatusPeriodically(ctxForUpdateSyncStatus, p, state)

	klog.V(3).Info("Updater starting...")
	start := time.Now()
	syncErrs := p.options().Update(ctx, &state.cache)
	metrics.RecordParserDuration(ctx, trigger, "update", metrics.StatusTagKey(syncErrs), start)
	klog.V(3).Info("Updater stopped")

	// This is to terminate `updateSyncStatusPeriodically`.
	cancel()

	klog.V(3).Info("Updating sync status (after sync)")
	if err := setSyncStatus(ctx, p, state, false, syncErrs); err != nil {
		syncErrs = status.Append(syncErrs, err)
	}

	return status.Append(sourceErrs, syncErrs)
}

// setSyncStatus updates `.status.sync` and the Syncing condition, if needed,
// as well as `state.syncStatus` and `state.syncingConditionLastUpdate` if
// the update is successful.
func setSyncStatus(ctx context.Context, p Parser, state *reconcilerState, syncing bool, syncErrs status.MultiError) error {
	// Update the RSync status, if necessary
	newSyncStatus := syncStatus{
		syncing:    syncing,
		commit:     state.cache.source.commit,
		errs:       syncErrs,
		lastUpdate: metav1.Now(),
	}
	if state.needToSetSyncStatus(newSyncStatus) {
		if err := p.SetSyncStatus(ctx, newSyncStatus); err != nil {
			return err
		}
		state.syncStatus = newSyncStatus
		state.syncingConditionLastUpdate = newSyncStatus.lastUpdate
	}

	// Extract conflict errors from sync errors.
	var conflictErrs []status.ManagementConflictError
	if syncErrs != nil {
		for _, err := range syncErrs.Errors() {
			if conflictErr, ok := err.(status.ManagementConflictError); ok {
				conflictErrs = append(conflictErrs, conflictErr)
			}
		}
	}
	// Report conflict errors to the remote manager, if it's a RootSync.
	if err := reportRootSyncConflicts(ctx, p.K8sClient(), conflictErrs); err != nil {
		return errors.Wrapf(err, "failed to report remote conflicts")
	}
	return nil
}

// updateSyncStatusPeriodically update the sync status periodically until the
// cancellation function of the context is called.
func updateSyncStatusPeriodically(ctx context.Context, p Parser, state *reconcilerState) {
	klog.V(3).Info("Periodic sync status updates starting...")
	updatePeriod := p.options().statusUpdatePeriod
	updateTimer := time.NewTimer(updatePeriod)
	defer updateTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			// ctx.Done() is closed when the cancellation function of the context is called.
			klog.V(3).Info("Periodic sync status updates stopped")
			return

		case <-updateTimer.C:
			klog.V(3).Info("Updating sync status (periodic while syncing)")
			if err := setSyncStatus(ctx, p, state, true, p.SyncErrors()); err != nil {
				klog.Warningf("failed to update sync status: %v", err)
			}

			updateTimer.Reset(updatePeriod) // Schedule status update attempt
		}
	}
}

// reportRootSyncConflicts reports conflicts to the RootSync that manages the
// conflicting resources.
func reportRootSyncConflicts(ctx context.Context, k8sClient client.Client, conflictErrs []status.ManagementConflictError) error {
	if len(conflictErrs) == 0 {
		return nil
	}
	conflictingManagerErrors := map[string][]status.ManagementConflictError{}
	for _, conflictError := range conflictErrs {
		conflictingManager := conflictError.ConflictingManager()
		err := conflictError.ConflictingManagerError()
		conflictingManagerErrors[conflictingManager] = append(conflictingManagerErrors[conflictingManager], err)
	}

	for conflictingManager, conflictErrors := range conflictingManagerErrors {
		scope, name := declared.ManagerScopeAndName(conflictingManager)
		if scope == declared.RootReconciler {
			// RootSync applier uses PolicyAdoptAll.
			// So it may fight, if the webhook is disabled.
			// Report the conflict to the other RootSync to make it easier to detect.
			klog.Infof("Detected conflict with RootSync manager %q", conflictingManager)
			if err := prependRootSyncRemediatorStatus(ctx, k8sClient, name, conflictErrors, defaultDenominator); err != nil {
				return errors.Wrapf(err, "failed to update RootSync %q to prepend remediator conflicts", name)
			}
		} else {
			// RepoSync applier uses PolicyAdoptIfNoInventory.
			// So it won't fight, even if the webhook is disabled.
			klog.Infof("Detected conflict with RepoSync manager %q", conflictingManager)
		}
	}
	return nil
}
