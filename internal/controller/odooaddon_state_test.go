/*
Copyright 2026 Odoo K8s Operator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	stateCurrentHash = "current-hash"
	stateOldHash     = "old-spec-hash"
	stateSpecErr     = "gitUrl scheme not allowed"
	stateAddonUID    = "addon-uid-1"
)

// stateNow is a fixed clock for the decision tests.
var stateNow = time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)

func stateAgo(d time.Duration) *time.Time {
	t := stateNow.Add(-d)
	return &t
}

// ownedJob is an observation of a Job this addon controls, built from the current spec.
func ownedJob(mutate func(*addonJobObservation)) addonJobObservation {
	o := addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

func TestDecideAddonSync(t *testing.T) {
	succeeded := func(o *addonJobObservation) { o.Succeeded = true }
	failed := func(o *addonJobObservation) { o.Failed = true }
	stale := func(o *addonJobObservation) { o.SpecHash = stateOldHash }
	foreign := func(o *addonJobObservation) { o.ControlledByAddon = false }

	tests := []struct {
		name string
		in   addonSyncInput
		want addonSyncDecision
	}{
		{
			name: "no job and nothing synced yet creates one and shows Syncing",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Now: stateNow},
			want: addonSyncDecision{Action: addonActionCreateJob, Phase: phaseSyncing, Requeue: addonCreateRequeue},
		},
		{
			name: "no job for a synced addon creates one without touching its phase",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, Now: stateNow},
			want: addonSyncDecision{Action: addonActionCreateJob, Requeue: addonCreateRequeue},
		},
		{
			name: "recent success is inside the resync cooldown",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, LastSyncTime: stateAgo(time.Minute), Now: stateNow},
			want: addonSyncDecision{Action: addonActionCooldown, Requeue: resyncInterval - time.Minute},
		},
		{
			name: "recent failure is inside the failure backoff",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseFailed, LastSyncTime: stateAgo(10 * time.Second), Now: stateNow},
			want: addonSyncDecision{Action: addonActionCooldown, Requeue: failureBackoff - 10*time.Second},
		},
		{
			name: "failure older than the backoff retries",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseFailed, LastSyncTime: stateAgo(45 * time.Second), Now: stateNow},
			want: addonSyncDecision{Action: addonActionCreateJob, Phase: phaseSyncing, Requeue: addonCreateRequeue},
		},
		{
			name: "success exactly one resync interval old is past the cooldown",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, LastSyncTime: stateAgo(resyncInterval), Now: stateNow},
			want: addonSyncDecision{Action: addonActionCreateJob, Requeue: addonCreateRequeue},
		},
		{
			name: "pinned and synced skips a resync",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, SkipResync: true, LastSyncTime: stateAgo(10 * time.Minute), Now: stateNow},
			want: addonSyncDecision{Action: addonActionSkipResync, Requeue: resyncInterval},
		},
		{
			name: "cooldown is checked before the pinned skip",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, SkipResync: true, LastSyncTime: stateAgo(time.Minute), Now: stateNow},
			want: addonSyncDecision{Action: addonActionCooldown, Requeue: resyncInterval - time.Minute},
		},
		{
			name: "running job is awaited and shows Syncing when not yet synced",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(nil), Now: stateNow},
			want: addonSyncDecision{Action: addonActionAwaitJob, Phase: phaseSyncing, Requeue: addonAwaitRequeue},
		},
		{
			name: "running job keeps a synced addon's phase",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, Job: ownedJob(nil), Now: stateNow},
			want: addonSyncDecision{Action: addonActionAwaitJob, Requeue: addonAwaitRequeue},
		},
		{
			name: "running job on a failed addon shows Syncing",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseFailed, Job: ownedJob(nil), Now: stateNow},
			want: addonSyncDecision{Action: addonActionAwaitJob, Phase: phaseSyncing, Requeue: addonAwaitRequeue},
		},
		{
			name: "running job ignores the cooldown",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, Job: ownedJob(nil), LastSyncTime: stateAgo(time.Second), Now: stateNow},
			want: addonSyncDecision{Action: addonActionAwaitJob, Requeue: addonAwaitRequeue},
		},
		{
			name: "succeeded job is recorded as Synced and retired",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSyncing, Job: ownedJob(succeeded), Now: stateNow},
			want: addonSyncDecision{Action: addonActionRecordSuccess, Phase: phaseSynced, DeleteJob: true, Requeue: resyncInterval},
		},
		{
			name: "failed job is recorded as Failed and retired",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSyncing, Job: ownedJob(failed), Now: stateNow},
			want: addonSyncDecision{Action: addonActionRecordFailure, Phase: phaseFailed, DeleteJob: true, Requeue: failureBackoff},
		},
		{
			name: "a job that both succeeded and failed counts as succeeded",
			in: addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSyncing, Now: stateNow,
				Job: ownedJob(func(o *addonJobObservation) { o.Succeeded, o.Failed = true, true })},
			want: addonSyncDecision{Action: addonActionRecordSuccess, Phase: phaseSynced, DeleteJob: true, Requeue: resyncInterval},
		},
		{
			name: "stale succeeded job is deleted and never recorded",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(func(o *addonJobObservation) { succeeded(o); stale(o) }), Now: stateNow},
			want: addonSyncDecision{Action: addonActionDeleteStaleJob, DeleteJob: true, Requeue: addonStaleJobRequeue},
		},
		{
			name: "stale failed job is deleted and never recorded",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(func(o *addonJobObservation) { failed(o); stale(o) }), Now: stateNow},
			want: addonSyncDecision{Action: addonActionDeleteStaleJob, DeleteJob: true, Requeue: addonStaleJobRequeue},
		},
		{
			name: "stale running job is deleted",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Phase: phaseSynced, Job: ownedJob(stale), Now: stateNow},
			want: addonSyncDecision{Action: addonActionDeleteStaleJob, DeleteJob: true, Requeue: addonStaleJobRequeue},
		},
		{
			name: "job without a spec hash is treated as stale",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(func(o *addonJobObservation) { o.SpecHash = "" }), Now: stateNow},
			want: addonSyncDecision{Action: addonActionDeleteStaleJob, DeleteJob: true, Requeue: addonStaleJobRequeue},
		},
		{
			name: "foreign job is a conflict and is not deleted",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(func(o *addonJobObservation) { foreign(o); succeeded(o) }), Now: stateNow},
			want: addonSyncDecision{Action: addonActionJobConflict, Phase: phaseFailed, Message: addonMsgJobConflict, Requeue: failureBackoff},
		},
		{
			name: "foreign stale job is still a conflict",
			in:   addonSyncInput{SpecHash: stateCurrentHash, Job: ownedJob(func(o *addonJobObservation) { foreign(o); stale(o) }), Now: stateNow},
			want: addonSyncDecision{Action: addonActionJobConflict, Phase: phaseFailed, Message: addonMsgJobConflict, Requeue: failureBackoff},
		},
		{
			name: "invalid spec with no job fails without creating one",
			in:   addonSyncInput{SpecErr: stateSpecErr, Phase: phaseSynced, Now: stateNow},
			want: addonSyncDecision{Action: addonActionInvalidSpec, Phase: phaseFailed, Message: stateSpecErr, Requeue: failureBackoff},
		},
		{
			name: "invalid spec deletes its own running job",
			in:   addonSyncInput{SpecErr: stateSpecErr, Job: ownedJob(nil), Now: stateNow},
			want: addonSyncDecision{Action: addonActionInvalidSpec, Phase: phaseFailed, Message: stateSpecErr, DeleteJob: true, Requeue: failureBackoff},
		},
		{
			name: "invalid spec leaves a foreign job alone",
			in:   addonSyncInput{SpecErr: stateSpecErr, Job: ownedJob(foreign), Now: stateNow},
			want: addonSyncDecision{Action: addonActionInvalidSpec, Phase: phaseFailed, Message: stateSpecErr, Requeue: failureBackoff},
		},
		{
			name: "invalid spec is checked before a succeeded job",
			in:   addonSyncInput{SpecErr: stateSpecErr, Job: ownedJob(succeeded), Now: stateNow},
			want: addonSyncDecision{Action: addonActionInvalidSpec, Phase: phaseFailed, Message: stateSpecErr, DeleteJob: true, Requeue: failureBackoff},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAddonSync(tc.in)
			if got != tc.want {
				t.Errorf("decideAddonSync() = %+v (%s), want %+v (%s)", got, got.Action, tc.want, tc.want.Action)
			}
		})
	}
}

func TestAddonSyncActionString(t *testing.T) {
	if got := addonActionCreateJob.String(); got != "CreateJob" {
		t.Errorf("String() = %q, want CreateJob", got)
	}
	if got := addonSyncAction(-1).String(); got != "Unknown" {
		t.Errorf("String() of out-of-range action = %q, want Unknown", got)
	}
}

func TestObserveSyncJob(t *testing.T) {
	addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS, UID: stateAddonUID}}
	owner := metav1.NewControllerRef(addon, odoov1.GroupVersion.WithKind("OdooAddon"))
	complete := []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	failedCond := []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}

	newJob := func(owned bool, hash string, mutate func(*batchv1.Job)) *batchv1.Job {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: syncJobName(testAddonName), Namespace: testAddonNS, UID: types.UID("job-uid"),
			Annotations: map[string]string{annotationSyncSpecHash: hash},
		}}
		if owned {
			job.OwnerReferences = []metav1.OwnerReference{*owner}
		}
		if mutate != nil {
			mutate(job)
		}
		return job
	}

	tests := []struct {
		name string
		job  *batchv1.Job
		want addonJobObservation
	}{
		{name: "missing job", job: nil, want: addonJobObservation{}},
		{
			name: "running owned job",
			job:  newJob(true, stateCurrentHash, nil),
			want: addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash},
		},
		{
			name: "foreign job",
			job:  newJob(false, stateCurrentHash, nil),
			want: addonJobObservation{Exists: true, SpecHash: stateCurrentHash},
		},
		{
			name: "succeeded by count",
			job:  newJob(true, stateCurrentHash, func(j *batchv1.Job) { j.Status.Succeeded = 1 }),
			want: addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash, Succeeded: true},
		},
		{
			name: "succeeded by Complete condition",
			job:  newJob(true, stateCurrentHash, func(j *batchv1.Job) { j.Status.Conditions = complete }),
			want: addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash, Succeeded: true},
		},
		{
			name: "failed pod but Job still retrying is not terminal",
			job:  newJob(true, stateCurrentHash, func(j *batchv1.Job) { j.Status.Failed = 1 }),
			want: addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash},
		},
		{
			name: "failed by Failed condition (deadline)",
			job:  newJob(true, stateCurrentHash, func(j *batchv1.Job) { j.Status.Conditions = failedCond }),
			want: addonJobObservation{Exists: true, ControlledByAddon: true, SpecHash: stateCurrentHash, Failed: true},
		},
		{
			name: "job with no hash annotation",
			job:  newJob(true, "", func(j *batchv1.Job) { j.Annotations = nil }),
			want: addonJobObservation{Exists: true, ControlledByAddon: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := observeSyncJob(tc.job, addon); got != tc.want {
				t.Errorf("observeSyncJob() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSyncSpecHash(t *testing.T) {
	base := testSpec(nil)
	if got := syncSpecHash(base); len(got) != syncSpecHashLen {
		t.Fatalf("syncSpecHash length = %d, want %d", len(got), syncSpecHashLen)
	}
	if a, b := syncSpecHash(base), syncSpecHash(testSpec(nil)); a != b {
		t.Errorf("hash is not deterministic: %s != %s", a, b)
	}

	// Every input that changes the Job output must change the hash.
	changes := map[string]func(*syncSpec){
		"gitUrl":      func(s *syncSpec) { s.GitURL = "https://github.com/acme/other.git" },
		"gitRef":      func(s *syncSpec) { s.GitRef = "release" },
		"addonPath":   func(s *syncSpec) { s.AddonPath = testAddonPath },
		"moduleName":  func(s *syncSpec) { s.ModuleName = "other_module" },
		"singleAddon": func(s *syncSpec) { s.SingleAddon = true },
		"secretName":  func(s *syncSpec) { s.SecretName = testSecretName },
		"addonName":   func(s *syncSpec) { s.AddonName = "other-addon" },
	}
	seen := map[string]string{syncSpecHash(base): tiEnvModule}
	for field, mutate := range changes {
		h := syncSpecHash(testSpec(mutate))
		if other, dup := seen[h]; dup {
			t.Errorf("changing %s gives the same hash as %s", field, other)
		}
		seen[h] = field
	}

	// Values that could blur together without separators must not collide.
	a := syncSpecHash(testSpec(func(s *syncSpec) { s.GitURL, s.GitRef = "ab", "c" }))
	b := syncSpecHash(testSpec(func(s *syncSpec) { s.GitURL, s.GitRef = "a", "bc" }))
	if a == b {
		t.Errorf("hash collides across field boundaries")
	}
}

func TestBuildSyncJobCarriesSpecHash(t *testing.T) {
	spec := testSpec(nil)
	addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS}}
	job := buildSyncJob(addon, spec, syncJobOptions{Image: testSyncImage, PVCName: testPVCName})
	if got := job.Annotations[annotationSyncSpecHash]; got != syncSpecHash(spec) {
		t.Errorf("annotation %s = %q, want %q", annotationSyncSpecHash, got, syncSpecHash(spec))
	}
}

func TestSyncingPhaseFor(t *testing.T) {
	if got := syncingPhaseFor(phaseSynced); got != "" {
		t.Errorf("synced addon phase = %q, want unchanged", got)
	}
	for _, phase := range []string{"", phasePending, phaseFailed} {
		if got := syncingPhaseFor(phase); got != phaseSyncing {
			t.Errorf("syncingPhaseFor(%q) = %q, want %q", phase, got, phaseSyncing)
		}
	}
}
