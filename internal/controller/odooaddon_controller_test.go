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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	oaEnvInstA        = "oa-env-inst-a"
	oaEnvInstB        = "oa-env-inst-b"
	oaEnvAddonA       = "oa-env-addon-a"
	oaEnvAddonB       = "oa-env-addon-b"
	oaEnvAddonC       = "oa-env-addon-c"
	oaEnvDatabase     = "odoo_env"
	oaEnvSecret       = "oa-env-pg"
	oaEnvRelease      = "release"
	oaEnvBadURL       = "ftp://example.com/addons.git"
	oaEnvFakeLogs     = "fake logs"
	oaEnvBackoffRsn   = "BackoffLimitExceeded"
	oaEnvBackoffMsg   = "Job has reached the specified backoff limit"
	oaEnvLegacyName   = "test-resource"
	oaEnvJobNameLabel = "job-name"
)

// oaEnvLogsFor returns a pod for job so the reconciler can read its logs. The fake
// clientset answers every log request with oaEnvFakeLogs, whatever the pod is.
func oaEnvLogsFor(job *batchv1.Job) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-logs-%s", job.Name, job.UID),
			Namespace: job.Namespace,
			Labels: map[string]string{
				batchControllerUIDLabel: string(job.UID),
				oaEnvJobNameLabel:       job.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: syncContainerName, Image: testSyncImage}},
		},
	}
}

// oaLogsClientset is a fake clientset whose pod logs are the given text. The stock fake
// always answers "fake logs", which cannot exercise the modules line of a sync Job.
type oaLogsClientset struct {
	kubernetes.Interface
	logs string
}

func (c oaLogsClientset) CoreV1() corev1client.CoreV1Interface {
	return oaLogsCoreV1{CoreV1Interface: c.Interface.CoreV1(), logs: c.logs}
}

type oaLogsCoreV1 struct {
	corev1client.CoreV1Interface
	logs string
}

func (c oaLogsCoreV1) Pods(namespace string) corev1client.PodInterface {
	return oaLogsPods{PodInterface: c.CoreV1Interface.Pods(namespace), logs: c.logs}
}

type oaLogsPods struct {
	corev1client.PodInterface
	logs string
}

// GetLogs returns a request whose body is the configured logs, whatever the pod is.
func (p oaLogsPods) GetLogs(string, *corev1.PodLogOptions) *restclient.Request {
	hc := &http.Client{Transport: oaLogsTransport(p.logs)}
	return restclient.NewRequestWithClient(&url.URL{Scheme: "http", Host: "logs.invalid"}, "", restclient.ClientContentConfig{}, hc)
}

type oaLogsTransport string

func (body oaLogsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}, nil
}

var _ = Describe("OdooAddon Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = oaEnvLegacyName

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: testNamespace,
		}
		odooaddon := &odoov1.OdooAddon{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OdooAddon")
			err := k8sClient.Get(ctx, typeNamespacedName, odooaddon)
			if err != nil && apierrors.IsNotFound(err) {
				addon := &odoov1.OdooAddon{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: testNamespace,
					},
				}
				Expect(k8sClient.Create(ctx, addon)).To(Succeed())
			}
		})

		AfterEach(func() {
			addon := &odoov1.OdooAddon{}
			err := k8sClient.Get(ctx, typeNamespacedName, addon)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OdooAddon")
			Expect(k8sClient.Delete(ctx, addon)).To(Succeed())
			// The reconcile above added the finalizer; run the deletion path so the
			// object actually goes away and does not leak into other specs.
			oaFinalize(ctx, resourceName)
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &OdooAddonReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("sync Job state machine", func() {
		ctx := context.Background()

		newReconciler := func(clientset kubernetes.Interface) *OdooAddonReconciler {
			return &OdooAddonReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Clientset: clientset,
			}
		}

		reconcile := func(r *OdooAddonReconciler, name string) ctrl.Result {
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		getAddon := func(name string) *odoov1.OdooAddon {
			addon := &odoov1.OdooAddon{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, addon)).To(Succeed())
			return addon
		}

		getJob := func(addonName string) (*batchv1.Job, error) {
			return getNamedJob(syncJobName(addonName))
		}

		// createInstance makes an OdooInstance; bound also creates its addons PVC as Bound,
		// which is what the reconciler waits for before it starts a sync.
		createInstance := func(name string, bound bool) {
			inst := &odoov1.OdooInstance{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: odoov1.OdooInstanceSpec{
					Postgres: odoov1.PostgresSpec{Database: oaEnvDatabase, PasswordSecret: oaEnvSecret},
				},
			}
			Expect(k8sClient.Create(ctx, inst)).To(Succeed())
			if !bound {
				return
			}
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: addonsPVCName(name), Namespace: testNamespace},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			pvc.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		}

		// createAddon makes an OdooAddon for instance, with the base spec adjusted by mutate.
		createAddon := func(name, instance string, mutate func(*odoov1.OdooAddonSpec)) {
			spec := odoov1.OdooAddonSpec{
				GitUrl:      testHTTPSURL,
				GitRef:      testBranchRef,
				InstanceRef: odoov1.OdooInstanceRef{Name: instance},
			}
			if mutate != nil {
				mutate(&spec)
			}
			addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Spec: spec}
			Expect(k8sClient.Create(ctx, addon)).To(Succeed())
		}

		// setJobStatus writes Job status the way the Job controller would for a finished Job.
		setJobStatus := func(name string, succeeded bool) {
			setNamedJobStatus(syncJobName(name), succeeded)
		}

		// cleanup removes everything the specs created. envtest has no garbage collector,
		// so Jobs are deleted with background propagation and addons are finalized.
		cleanup := func(addons []string, instances []string) {
			background := client.PropagationPolicy(metav1.DeletePropagationBackground)
			for _, name := range addons {
				_ = k8sClient.Delete(ctx, &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
				oaFinalize(ctx, name)
				_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: syncJobName(name), Namespace: testNamespace}}, background)
				_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: cleanupJobName(name), Namespace: testNamespace}}, background)
				_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(testNamespace), client.MatchingLabels{oaEnvJobNameLabel: syncJobName(name)})
			}
			for _, name := range instances {
				_ = k8sClient.Delete(ctx, &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
				oaDeletePVC(ctx, addonsPVCName(name))
			}
		}

		Context("when an addon is created for a bound instance", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, []string{oaEnvInstA})
			})

			It("starts the sync Job and returns without blocking", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)

				start := time.Now()
				res := reconcile(newReconciler(nil), oaEnvAddonA)
				Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
				Expect(res.RequeueAfter).To(Equal(addonCreateRequeue))

				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(job.OwnerReferences).To(HaveLen(1))
				Expect(*job.OwnerReferences[0].Controller).To(BeTrue())
				Expect(job.Annotations).To(HaveKeyWithValue(annotationSyncSpecHash, oaEnvSpecHash(oaEnvAddonA, testBranchRef)))

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseSyncing))
				Expect(addon.Status.Ready).To(BeFalse())
			})

			It("waits for the addons volume to be bound before starting a Job", func() {
				createInstance(oaEnvInstA, false)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)

				res := reconcile(newReconciler(nil), oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(pendingRequeue))
				Expect(getAddon(oaEnvAddonA).Status.Phase).To(Equal(phasePending))
				_, err := getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Job may exist before the volume is bound")
			})

			It("retires the sync Job, runs a cleanup Job, then removes the finalizer", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(nil)
				reconcile(r, oaEnvAddonA)

				Expect(k8sClient.Delete(ctx, getAddon(oaEnvAddonA))).To(Succeed())
				res := reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonStaleJobRequeue))
				_, err := getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the sync Job is stopped first")

				res = reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonCleanupRequeue))
				cleanupJob, err := getNamedJob(cleanupJobName(oaEnvAddonA))
				Expect(err).NotTo(HaveOccurred())
				Expect(cleanupJob.OwnerReferences).To(HaveLen(1))
				Expect(*cleanupJob.OwnerReferences[0].Controller).To(BeTrue())

				res = reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonCleanupRequeue), "still running: keep waiting")
				Expect(getAddon(oaEnvAddonA).Finalizers).To(ContainElement(finalizerOdooAddon))

				setNamedJobStatus(cleanupJobName(oaEnvAddonA), true)
				reconcile(r, oaEnvAddonA)

				err = k8sClient.Get(ctx, types.NamespacedName{Name: oaEnvAddonA, Namespace: testNamespace}, &odoov1.OdooAddon{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the addon is gone once its finalizer is removed")
				_, err = getNamedJob(cleanupJobName(oaEnvAddonA))
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a finished cleanup Job is retired")
			})

			It("removes the finalizer anyway when the cleanup Job fails", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(nil)
				reconcile(r, oaEnvAddonA)
				Expect(k8sClient.Delete(ctx, getAddon(oaEnvAddonA))).To(Succeed())
				reconcile(r, oaEnvAddonA)
				reconcile(r, oaEnvAddonA)

				setNamedJobStatus(cleanupJobName(oaEnvAddonA), false)
				reconcile(r, oaEnvAddonA)

				err := k8sClient.Get(ctx, types.NamespacedName{Name: oaEnvAddonA, Namespace: testNamespace}, &odoov1.OdooAddon{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "deletion must never stick on a failed cleanup")
			})

			It("skips the cleanup Job when the addons PVC is gone", func() {
				createInstance(oaEnvInstA, false)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(nil)
				reconcile(r, oaEnvAddonA)

				Expect(k8sClient.Delete(ctx, getAddon(oaEnvAddonA))).To(Succeed())
				reconcile(r, oaEnvAddonA)

				err := k8sClient.Get(ctx, types.NamespacedName{Name: oaEnvAddonA, Namespace: testNamespace}, &odoov1.OdooAddon{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				_, err = getNamedJob(cleanupJobName(oaEnvAddonA))
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no cleanup Job without a volume")
			})
		})

		Context("when the addon is deleted before its instance exists", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, nil)
			})

			It("removes the finalizer immediately", func() {
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(nil)
				reconcile(r, oaEnvAddonA)

				Expect(k8sClient.Delete(ctx, getAddon(oaEnvAddonA))).To(Succeed())
				reconcile(r, oaEnvAddonA)

				err := k8sClient.Get(ctx, types.NamespacedName{Name: oaEnvAddonA, Namespace: testNamespace}, &odoov1.OdooAddon{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				_, err = getNamedJob(cleanupJobName(oaEnvAddonA))
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no cleanup Job without an instance")
			})
		})

		Context("when the sync Job succeeds", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, []string{oaEnvInstA})
			})

			It("records the synced commit, marks the addon Ready and retires the Job", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				clientset := fake.NewClientset()
				r := newReconciler(clientset)
				reconcile(r, oaEnvAddonA)

				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())
				setJobStatus(oaEnvAddonA, true)

				res := reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(resyncInterval))

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseSynced))
				Expect(addon.Status.Ready).To(BeTrue())
				Expect(addon.Status.ClonedCommit).To(Equal(oaEnvFakeLogs))
				Expect(addon.Status.LastSyncTime).NotTo(BeNil())
				Expect(addon.Status.Message).To(BeEmpty())

				_, err = getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a finished Job is retired")
			})

			It("records the modules the sync published next to the commit", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				logs := "modules: website_theme,sale_theme\n" + testSHARef + "\n"
				r := newReconciler(oaLogsClientset{Interface: fake.NewClientset(), logs: logs})
				reconcile(r, oaEnvAddonA)
				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())
				setJobStatus(oaEnvAddonA, true)

				reconcile(r, oaEnvAddonA)

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseSynced))
				Expect(addon.Status.ClonedCommit).To(Equal(testSHARef))
				Expect(addon.Status.Modules).To(Equal([]string{"sale_theme", "website_theme"}))
			})

			It("still succeeds, with no modules, when the sync prints no modules line", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(oaLogsClientset{Interface: fake.NewClientset(), logs: testSHARef + "\n"})
				reconcile(r, oaEnvAddonA)
				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())
				setJobStatus(oaEnvAddonA, true)

				reconcile(r, oaEnvAddonA)

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseSynced))
				Expect(addon.Status.ClonedCommit).To(Equal(testSHARef))
				Expect(addon.Status.Modules).To(BeEmpty())
			})

			It("keeps a previously synced addon Ready and Synced while a resync Job runs", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(fake.NewClientset())
				reconcile(r, oaEnvAddonA)
				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())
				setJobStatus(oaEnvAddonA, true)
				reconcile(r, oaEnvAddonA)

				// Make the cooldown expire so the next pass starts a resync.
				addon := getAddon(oaEnvAddonA)
				expired := metav1.NewTime(time.Now().Add(-2 * resyncInterval))
				addon.Status.LastSyncTime = &expired
				Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())

				res := reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonCreateRequeue))
				_, err = getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred(), "the resync Job exists")

				res = reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonAwaitRequeue))

				addon = getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseSynced))
				Expect(addon.Status.Ready).To(BeTrue())
				Expect(addon.Status.ClonedCommit).To(Equal(oaEnvFakeLogs))
			})
		})

		Context("when the sync Job fails", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, []string{oaEnvInstA})
			})

			It("marks the addon Failed with the reason and log tail, and retires the Job", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(fake.NewClientset())
				reconcile(r, oaEnvAddonA)
				job, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())

				setJobStatus(oaEnvAddonA, false)
				res := reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(failureBackoff))

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseFailed))
				Expect(addon.Status.Ready).To(BeFalse())
				Expect(addon.Status.Message).To(ContainSubstring(oaEnvBackoffRsn))
				Expect(addon.Status.Message).To(ContainSubstring(oaEnvFakeLogs))
				Expect(addon.Status.LastSyncTime).NotTo(BeNil())

				_, err = getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})
		})

		Context("when the spec changes while a Job is running", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, []string{oaEnvInstA})
			})

			It("deletes the Job built from the old spec and creates one for the new spec", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				r := newReconciler(nil)
				reconcile(r, oaEnvAddonA)
				oldJob, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(oldJob.Annotations[annotationSyncSpecHash]).To(Equal(oaEnvSpecHash(oaEnvAddonA, testBranchRef)))

				addon := getAddon(oaEnvAddonA)
				addon.Spec.GitRef = oaEnvRelease
				Expect(k8sClient.Update(ctx, addon)).To(Succeed())

				res := reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonStaleJobRequeue))
				_, err = getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the stale Job is deleted")

				res = reconcile(r, oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(addonCreateRequeue))
				newJob, err := getJob(oaEnvAddonA)
				Expect(err).NotTo(HaveOccurred())
				Expect(newJob.Annotations[annotationSyncSpecHash]).To(Equal(oaEnvSpecHash(oaEnvAddonA, oaEnvRelease)))
			})
		})

		Context("when several addons share one instance", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA, oaEnvAddonB}, []string{oaEnvInstA})
			})

			It("gives each addon its own Job in one pass each, without serializing", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, nil)
				createAddon(oaEnvAddonB, oaEnvInstA, nil)
				r := newReconciler(nil)

				for _, name := range []string{oaEnvAddonA, oaEnvAddonB} {
					start := time.Now()
					res := reconcile(r, name)
					Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
					Expect(res.RequeueAfter).To(Equal(addonCreateRequeue))
				}

				for _, name := range []string{oaEnvAddonA, oaEnvAddonB} {
					job, err := getJob(name)
					Expect(err).NotTo(HaveOccurred())
					Expect(job.Name).To(Equal(syncJobName(name)))
				}
			})
		})

		Context("when a Job with the sync name is not owned by the addon", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonC}, []string{oaEnvInstA})
			})

			It("fails the addon with a conflict and leaves the foreign Job alone", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonC, oaEnvInstA, nil)
				addon := getAddon(oaEnvAddonC)
				foreign := buildSyncJob(addon, testSpec(nil), syncJobOptions{Image: testSyncImage, PVCName: addonsPVCName(oaEnvInstA)})
				foreign.Name = syncJobName(oaEnvAddonC)
				Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

				reconcile(newReconciler(nil), oaEnvAddonC)

				addon = getAddon(oaEnvAddonC)
				Expect(addon.Status.Phase).To(Equal(phaseFailed))
				Expect(addon.Status.Message).To(Equal(addonMsgJobConflict))

				job, err := getJob(oaEnvAddonC)
				Expect(err).NotTo(HaveOccurred())
				Expect(job.OwnerReferences).To(BeEmpty())
			})
		})

		Context("when the spec is invalid", func() {
			AfterEach(func() {
				cleanup([]string{oaEnvAddonA}, []string{oaEnvInstA})
			})

			It("fails the addon without creating a Job", func() {
				createInstance(oaEnvInstA, true)
				createAddon(oaEnvAddonA, oaEnvInstA, func(s *odoov1.OdooAddonSpec) { s.GitUrl = oaEnvBadURL })

				res := reconcile(newReconciler(nil), oaEnvAddonA)
				Expect(res.RequeueAfter).To(Equal(failureBackoff))

				addon := getAddon(oaEnvAddonA)
				Expect(addon.Status.Phase).To(Equal(phaseFailed))
				Expect(addon.Status.Message).To(ContainSubstring("gitUrl scheme not allowed"))
				_, err := getJob(oaEnvAddonA)
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})
		})
	})
})

// oaEnvSpecHash is the hash the Job for addonName carries when the addon syncs gitRef.
func oaEnvSpecHash(addonName, gitRef string) string {
	spec, err := resolveSyncSpec(&odoov1.OdooAddon{
		ObjectMeta: metav1.ObjectMeta{Name: addonName, Namespace: testNamespace},
		Spec:       odoov1.OdooAddonSpec{GitUrl: testHTTPSURL, GitRef: gitRef},
	})
	Expect(err).NotTo(HaveOccurred())
	return syncSpecHash(spec)
}

// oaDeletePVC deletes a PVC for good. The apiserver adds the pvc-protection finalizer and
// only the PVC controller removes it, which envtest does not run, so the finalizer is
// cleared by hand; otherwise the name stays taken for the rest of the suite.
func oaDeletePVC(ctx context.Context, name string) {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: name, Namespace: testNamespace}
	if err := k8sClient.Get(ctx, key, pvc); err != nil {
		return
	}
	_ = k8sClient.Delete(ctx, pvc)
	if err := k8sClient.Get(ctx, key, pvc); err != nil {
		return
	}
	pvc.Finalizers = nil
	_ = k8sClient.Update(ctx, pvc)
}

// oaFinalize drops the finalizer of an addon that was deleted, so the object is actually
// removed without running the volume cleanup. Missing addons are ignored.
func oaFinalize(ctx context.Context, name string) {
	addon := &odoov1.OdooAddon{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, addon); err != nil {
		return
	}
	if !controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		return
	}
	controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
	_ = k8sClient.Update(ctx, addon)
}

// getNamedJob fetches a Job in the test namespace by name.
func getNamedJob(name string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNamespace}, job)
	return job, err
}

// setNamedJobStatus writes Job status the way the Job controller would for a finished Job.
func setNamedJobStatus(name string, succeeded bool) {
	job, err := getNamedJob(name)
	Expect(err).NotTo(HaveOccurred())
	now := metav1.Now()
	job.Status.StartTime = &now
	if succeeded {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "CompletionsReached"},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: oaEnvBackoffRsn, Message: oaEnvBackoffMsg},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: oaEnvBackoffRsn, Message: oaEnvBackoffMsg},
		}
	}
	Expect(k8sClient.Status().Update(context.Background(), job)).To(Succeed())
}
