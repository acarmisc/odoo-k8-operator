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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tbCompletedReason = "CompletionsReached"
	tbInstA           = "bk-env-inst-a"
	tbInstB           = "bk-env-inst-b"
	tbFirst           = "bk-env-first"
	tbSecond          = "bk-env-second"
	tbCron            = "bk-env-cron"
	tbDatabase        = "odoo_bk"
	tbSecret          = "bk-env-pg"
	tbClaim           = "bk-env-claim"
	tbClaimMoved      = "bk-env-claim-2"
	tbSchedule        = "30 1 * * *"
	tbLogLine         = "fake logs"
	tbBackoffRsn      = "BackoffLimitExceeded"
	tbBackoffMsg      = "Job has reached the specified backoff limit"
)

var _ = Describe("OdooBackup Controller", func() {
	ctx := context.Background()

	newReconciler := func(clientset kubernetes.Interface) *OdooBackupReconciler {
		return &OdooBackupReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Clientset: clientset,
		}
	}

	reconcile := func(r *OdooBackupReconciler, name string) ctrl.Result {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	getBackup := func(name string) *odoov1.OdooBackup {
		bk := &odoov1.OdooBackup{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, bk)).To(Succeed())
		return bk
	}

	getJob := func(name string) (*batchv1.Job, error) {
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, job)
		return job, err
	}

	createInstance := func(name string) {
		inst := &odoov1.OdooInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: odoov1.OdooInstanceSpec{
				Postgres: odoov1.PostgresSpec{Database: tbDatabase, PasswordSecret: tbSecret},
			},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
	}

	createBackup := func(name, instance string, mutate func(*odoov1.OdooBackupSpec)) {
		spec := odoov1.OdooBackupSpec{
			InstanceRef: odoov1.OdooInstanceRef{Name: instance},
			Destination: odoov1.BackupDestination{
				PVC: &odoov1.BackupPVCDestination{ClaimName: tbClaim},
			},
		}
		if mutate != nil {
			mutate(&spec)
		}
		bk := &odoov1.OdooBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Spec: spec}
		Expect(k8sClient.Create(ctx, bk)).To(Succeed())
	}

	// setJobStatus writes Job status the way the Job controller would for a finished Job.
	setJobStatus := func(name string, succeeded bool) {
		job, err := getJob(name)
		Expect(err).NotTo(HaveOccurred())
		now := metav1.Now()
		job.Status.StartTime = &now
		if succeeded {
			job.Status.Succeeded = 1
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: tbCompletedReason},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
		} else {
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: tbBackoffRsn, Message: tbBackoffMsg},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: tbBackoffRsn, Message: tbBackoffMsg},
			}
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	// cleanup removes everything a spec created. Background propagation deletes Jobs outright,
	// because envtest has no garbage collector.
	cleanup := func(backups []string, instances []string) {
		background := client.PropagationPolicy(metav1.DeletePropagationBackground)
		for _, name := range backups {
			_ = k8sClient.Delete(ctx, &odoov1.OdooBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
			_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: backupJobName(name), Namespace: testNamespace}}, background)
			_ = k8sClient.Delete(ctx, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: backupCronJobName(name), Namespace: testNamespace}}, background)
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: backupJobName(name) + "-pod", Namespace: testNamespace}})
		}
		for _, name := range instances {
			_ = k8sClient.Delete(ctx, &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
		}
	}

	Context("when a one-shot backup is created", func() {
		AfterEach(func() {
			cleanup([]string{tbFirst}, []string{tbInstA})
		})

		It("creates a Job with the data, destination and credential wiring and marks it Running", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)

			res := reconcile(newReconciler(nil), tbFirst)
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			bk := getBackup(tbFirst)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhaseRunning))
			Expect(bk.Status.JobName).To(Equal(backupJobName(tbFirst)))
			Expect(bk.Status.StartTime).NotTo(BeNil())
			Expect(bk.Status.ObservedGeneration).To(Equal(bk.Generation))

			job, err := getJob(bk.Status.JobName)
			Expect(err).NotTo(HaveOccurred())
			Expect(*job.Spec.BackoffLimit).To(BeZero())
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(backupDefaultTimeout))
			Expect(job.Spec.Template.Labels).NotTo(HaveKey(labelInstance))
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("OdooBackup"))

			spec := job.Spec.Template.Spec
			Expect(spec.Containers).To(HaveLen(1))
			Expect(spec.Containers[0].Command).To(HaveLen(3))
			Expect(spec.Volumes).To(ContainElement(HaveField("Name", volumeData)))
			Expect(spec.Volumes).To(ContainElement(HaveField("Name", volumeDestination)))
			Expect(spec.Containers[0].Env).To(ContainElement(HaveField("Name", envPGPassword)))
		})

		It("adopts an existing Job when the status was lost", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)
			r := newReconciler(nil)
			reconcile(r, tbFirst)

			bk := getBackup(tbFirst)
			bk.Status = odoov1.OdooBackupStatus{}
			Expect(k8sClient.Status().Update(ctx, bk)).To(Succeed())

			reconcile(r, tbFirst)
			Expect(getBackup(tbFirst).Status.Phase).To(Equal(odoov1.BackupPhaseRunning))
		})

		It("marks the backup Succeeded once the Job completes", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)
			r := newReconciler(nil)
			reconcile(r, tbFirst)

			setJobStatus(backupJobName(tbFirst), true)
			reconcile(r, tbFirst)

			bk := getBackup(tbFirst)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhaseSucceeded))
			Expect(bk.Status.CompletionTime).NotTo(BeNil())
			Expect(bk.Status.Conditions).To(ContainElement(HaveField("Type", odoov1.ConditionTypeSucceeded)))
		})

		It("is terminal: reconciling a Succeeded backup changes nothing", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)
			r := newReconciler(nil)
			reconcile(r, tbFirst)
			setJobStatus(backupJobName(tbFirst), true)
			reconcile(r, tbFirst)

			reconcile(r, tbFirst)
			Expect(getBackup(tbFirst).Status.Phase).To(Equal(odoov1.BackupPhaseSucceeded))
		})

		It("rejects the spec change of a one-shot backup", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)

			bk := getBackup(tbFirst)
			bk.Spec.Destination.PVC.ClaimName = tbClaimMoved
			err := k8sClient.Update(ctx, bk)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		})
	})

	Context("when the Job fails", func() {
		AfterEach(func() {
			cleanup([]string{tbSecond}, []string{tbInstB})
		})

		It("marks the backup Failed with the condition message and the pod log tail", func() {
			createInstance(tbInstB)
			createBackup(tbSecond, tbInstB, nil)
			clientset := fake.NewClientset()
			r := newReconciler(clientset)
			reconcile(r, tbSecond)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupJobName(tbSecond) + "-pod",
					Namespace: testNamespace,
					Labels:    backupLabels(tbSecond),
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: backupMainContainer, Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			setJobStatus(backupJobName(tbSecond), false)
			reconcile(r, tbSecond)

			bk := getBackup(tbSecond)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhaseFailed))
			Expect(bk.Status.CompletionTime).NotTo(BeNil())
			Expect(bk.Status.Message).To(ContainSubstring(tbBackoffRsn))
			Expect(bk.Status.Message).To(ContainSubstring(tbLogLine))
		})
	})

	Context("when several backups target the same instance", func() {
		AfterEach(func() {
			cleanup([]string{tbFirst, tbSecond}, []string{tbInstA})
		})

		It("keeps the second backup Pending while the first is Running", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, nil)
			r := newReconciler(nil)
			reconcile(r, tbFirst)

			createBackup(tbSecond, tbInstA, nil)
			res := reconcile(r, tbSecond)
			Expect(res.RequeueAfter).To(Equal(backupRequeuePending))

			second := getBackup(tbSecond)
			Expect(second.Status.Phase).To(Equal(odoov1.BackupPhasePending))
			Expect(second.Status.Message).To(ContainSubstring(tbFirst))
			_, err := getJob(backupJobName(tbSecond))
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Job may exist for a queued backup")

			setJobStatus(backupJobName(tbFirst), true)
			reconcile(r, tbFirst)
			reconcile(r, tbSecond)
			Expect(getBackup(tbSecond).Status.Phase).To(Equal(odoov1.BackupPhaseRunning))
		})
	})

	Context("when the spec is invalid or the instance is missing", func() {
		AfterEach(func() {
			cleanup([]string{tbFirst}, []string{tbInstA})
		})

		It("waits for the instance before starting", func() {
			createBackup(tbFirst, "bk-env-missing", nil)
			res := reconcile(newReconciler(nil), tbFirst)
			Expect(res.RequeueAfter).To(Equal(backupRequeuePending))

			bk := getBackup(tbFirst)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhasePending))
			Expect(bk.Status.Message).To(Equal(backupMsgWaitingInstance))
		})

		It("fails an invalid destination without creating a Job", func() {
			createInstance(tbInstA)
			createBackup(tbFirst, tbInstA, func(s *odoov1.OdooBackupSpec) {
				s.Destination.PVC.SubPath = "../escape"
			})
			reconcile(newReconciler(nil), tbFirst)

			bk := getBackup(tbFirst)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhaseFailed))
			Expect(bk.Status.JobName).To(BeEmpty())
			_, err := getJob(backupJobName(tbFirst))
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("when the backup is scheduled", func() {
		AfterEach(func() {
			cleanup([]string{tbCron}, []string{tbInstA})
		})

		It("creates a CronJob and updates it when the spec changes", func() {
			createInstance(tbInstA)
			createBackup(tbCron, tbInstA, func(s *odoov1.OdooBackupSpec) {
				s.Schedule = tbSchedule
			})
			r := newReconciler(nil)
			reconcile(r, tbCron)

			bk := getBackup(tbCron)
			Expect(bk.Status.Phase).To(Equal(odoov1.BackupPhaseScheduled))
			Expect(bk.Status.CronJobName).To(Equal(backupCronJobName(tbCron)))

			cron := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupCronJobName(tbCron), Namespace: testNamespace}, cron)).To(Succeed())
			Expect(cron.Spec.Schedule).To(Equal(tbSchedule))
			Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
			Expect(cron.OwnerReferences).To(HaveLen(1))
			oldHash := cron.Annotations[backupHashAnnotation]

			// A scheduled backup may change: move the claim, the CronJob follows.
			bk = getBackup(tbCron)
			bk.Spec.Destination.PVC.ClaimName = tbClaimMoved
			Expect(k8sClient.Update(ctx, bk)).To(Succeed())
			reconcile(r, tbCron)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupCronJobName(tbCron), Namespace: testNamespace}, cron)).To(Succeed())
			Expect(cron.Annotations[backupHashAnnotation]).NotTo(Equal(oldHash))
			Expect(cron.Spec.JobTemplate.Spec.Template.Spec.Volumes).To(ContainElement(
				HaveField("Name", volumeDestination)))
		})

		It("rejects a scheduled backup that cannot become one-shot again", func() {
			createInstance(tbInstA)
			createBackup(tbCron, tbInstA, func(s *odoov1.OdooBackupSpec) {
				s.Schedule = tbSchedule
			})
			bk := getBackup(tbCron)
			bk.Spec.Schedule = ""
			err := k8sClient.Update(ctx, bk)
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		})
	})
})
