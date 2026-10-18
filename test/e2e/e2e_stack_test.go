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

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/odoo-k8s/odoo-k8-operator/test/utils"
)

// The full-stack scenario runs a real PostgreSQL and a real Odoo image on the kind cluster.
// Its steps are Ordered: a failing step skips the rest, and AfterEach dumps diagnostics.
// See docs/development.md (section "End-to-end tests on kind") for the list of steps.

const (
	kubectlBin   = "kubectl"
	pollInterval = 5 * time.Second

	// Objects of the scenario. The operator names its own objects after the instance.
	pgName         = "postgres"
	pgDatabase     = "e2e"
	pgUser         = "odoo"
	pgDBSecret     = "odoo-db"
	pgAdminSecret  = "odoo-admin"
	loginSecret    = "odoo-login"
	instanceName   = "e2e"
	cronDeployName = "e2e-cron"
	configMapName  = "e2e-config"
	initUpgrade    = "e2e-init"
	installUpgrade = "e2e-install"
	addonName      = "e2e-probe"
	addonModule    = "e2e_probe"
	addonSourceDir = "e2e-addons"
	backupName     = "e2e-backup"
	backupPVCName  = "e2e-backups"
	dataPVCName    = "e2e-data"
	addonsDir      = "/mnt/odoo/addons"
	webContainer   = "odoo"
	// renderedConfPath is the config the web container reads when adminPasswordSecret is set.
	renderedConfPath = "/run/odoo-conf/odoo.conf"
	odooImage        = "odoo:19.0"
	pgImage          = "postgres:17"
	curlImage        = "curlimages/curl:8.11.1"
	busyboxImage     = "busybox:1.36"

	// Phases and conditions reported by the operator.
	phaseRunning   = "Running"
	phaseSucceeded = "Succeeded"
	phaseSynced    = "Synced"
	phaseFailed    = "Failed"
	valueTrue      = "True"
	valueTrueLower = "true"
	podPhaseDone   = "Succeeded"
	podPhaseFailed = "Failed"
	envGitSHA      = "E2E_GIT_SHA"

	// Timeouts. Each one is the upper bound of a single step; a failing step ends the scenario.
	postgresTimeout = 4 * time.Minute
	initTimeout     = 15 * time.Minute // includes pulling the ~1.5 GB Odoo image
	webTimeout      = 5 * time.Minute
	loginTimeout    = 4 * time.Minute
	cronTimeout     = 5 * time.Minute
	addonTimeout    = 5 * time.Minute
	upgradeTimeout  = 8 * time.Minute
	backupTimeout   = 8 * time.Minute
	deleteTimeout   = 5 * time.Minute
	podTimeout      = 4 * time.Minute
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// stackState carries values from one step of the scenario to the next.
type stackState struct {
	gitSHA      string
	addonSynced bool
}

// describeFullStack adds the full-stack scenario to the Manager container. It is nested there so
// that the controller-manager is deployed while it runs and is removed by the Manager AfterAll.
func describeFullStack() {
	Context("full stack on PostgreSQL and Odoo", Ordered, func() {
		s := &stackState{}

		AfterEach(func() {
			if CurrentSpecReport().Failed() {
				By("dumping diagnostics of the failed step")
				dumpDiagnostics()
			}
		})

		AfterAll(func() {
			By("deleting the scenario objects, volumes and secrets")
			cleanupStack()
		})

		It("1. runs PostgreSQL with TLS and the odoo role", func() {
			By("creating the secrets with random passwords")
			Expect(createSecrets()).To(Succeed())

			By("deploying PostgreSQL with a self-signed certificate")
			Expect(utils.ApplyManifest(postgresManifest())).To(Succeed())
			waitForValue("deployment", pgName, "{.status.readyReplicas}", "1", postgresTimeout)
		})

		It("2. creates the OdooInstance with spec.initialize", func() {
			By("applying the OdooInstance " + instanceName)
			Expect(utils.ApplyManifest(instanceManifest())).To(Succeed())
		})

		It("3. initialises the database through the OdooUpgrade "+initUpgrade, func() {
			By("waiting for the init OdooUpgrade to succeed")
			waitForPhase("odooupgrade", initUpgrade, phaseSucceeded, phaseFailed, initTimeout)

			By("waiting for the instance to be initialised and running")
			waitForValue("odooinstance", instanceName, "{.status.initialized}", valueTrueLower, webTimeout)
			waitForValue("odooinstance", instanceName, "{.status.phase}", phaseRunning, webTimeout)
			waitForValue("odooinstance", instanceName, "{.status.ready}", valueTrueLower, webTimeout)
		})

		It("4. serves HTTP and accepts the generated admin password only", func() {
			By("checking that the web container reads the rendered config, without printing it")
			// grep -c prints only the number of matching lines; the file and the password stay out of the log.
			count, err := utils.Output(kubectlNS("exec", "deploy/"+instanceName, "-c", webContainer, "--",
				"sh", "-c", "grep -c '^admin_passwd = ' "+renderedConfPath))
			Expect(err).NotTo(HaveOccurred(), "the render-config init container must have written "+renderedConfPath)
			Expect(count).To(Equal("1"))

			By("checking that the web pods do not receive the master password in their environment")
			envNames, err := utils.Output(kubectlNS("get", "pods", "-l", "app=odoo,instance="+instanceName,
				"-o", `jsonpath={.items[*].spec.containers[?(@.name=="odoo")].env[*].name}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Fields(envNames)).To(ContainElement("PGPASSWORD"), "the env query must see the web container")
			Expect(strings.Fields(envNames)).NotTo(ContainElement("ODOO_ADMIN_PASSWD"))

			By("logging in through a curl pod against the web Service")
			Eventually(func(g Gomega) {
				out, err := runCheckPod(loginCheckPod(), podTimeout)
				g.Expect(err).NotTo(HaveOccurred())
				fields := parseKeyValueLines(out)
				g.Expect(fields["LOGIN_HTTP"]).To(Equal("200"), "GET /web/login")
				g.Expect(isLoggedIn(fields["AUTH_GOOD"])).To(BeTrue(),
					"login with the generated password failed: %s", fields["AUTH_GOOD"])
				g.Expect(isLoggedIn(fields["AUTH_DEFAULT"])).To(BeFalse(),
					"password 'admin' still logs in: %s", fields["AUTH_DEFAULT"])
			}).WithTimeout(loginTimeout).WithPolling(pollInterval).Should(Succeed())
		})

		It("5. runs the cron pod on its own Deployment", func() {
			By("enabling spec.cron")
			_, err := utils.Output(kubectlNS("patch", "odooinstance", instanceName, "--type=merge",
				"-p", `{"spec":{"cron":{"enabled":true}}}`))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cron Deployment and the CronReady condition")
			waitForValue("deployment", cronDeployName, "{.status.readyReplicas}", "1", cronTimeout)
			waitForValue("odooinstance", instanceName,
				`{.status.conditions[?(@.type=="CronReady")].status}`, valueTrue, cronTimeout)

			By("checking that the web config disables cron threads")
			cfg, err := utils.Output(kubectlNS("get", "configmap", configMapName, "-o", `jsonpath={.data.odoo\.conf}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).To(ContainSubstring("max_cron_threads = 0"))

			By("waiting for the web Deployment to roll out and become ready")
			_, err = utils.Output(kubectlNS("rollout", "status", "deployment/"+instanceName, "--timeout=5m"))
			Expect(err).NotTo(HaveOccurred())
			waitForValue("odooinstance", instanceName,
				`{.status.conditions[?(@.type=="Ready")].status}`, valueTrue, webTimeout)

			By("checking that the Service selects no cron pod")
			webIPs := serviceIPs()
			cronIPs := cronPodIPs()
			Expect(cronIPs).NotTo(BeEmpty(), "no cron pod has an IP")
			for _, ip := range cronIPs {
				Expect(webIPs).NotTo(ContainElement(ip), "the web Service selects cron pod %s", ip)
			}
		})

		It("6. syncs the e2e_probe addon from git at the tested commit", func() {
			s.gitSHA = os.Getenv(envGitSHA)
			if s.gitSHA == "" {
				Skip(envGitSHA + " is not set: skipping the addon steps (the commit must be pushed)")
			}
			Expect(shaPattern.MatchString(s.gitSHA)).To(BeTrue(), "%s must be a full commit SHA, got %q",
				envGitSHA, s.gitSHA)

			By("creating OdooAddon " + addonName + " at " + s.gitSHA)
			Expect(utils.ApplyManifest(addonManifest(s.gitSHA))).To(Succeed())
			waitForValue("odooaddon", addonName, "{.status.phase}", phaseSynced, addonTimeout)
			s.addonSynced = true

			By("checking the reported modules, commit and instance paths")
			waitForValue("odooaddon", addonName, "{.status.modules[*]}", addonModule, addonTimeout)
			waitForValue("odooaddon", addonName, "{.status.clonedCommit}", s.gitSHA, addonTimeout)
			waitForValue("odooinstance", instanceName, "{.status.addonPaths[*]}",
				addonsDir+"/"+addonModule, addonTimeout)

			By("checking the symlink in the web pod")
			Eventually(func(g Gomega) {
				out, err := listAddonsDir()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(MatchRegexp(addonModule+` -> `), "symlink missing:\n%s", out)
			}).WithTimeout(addonTimeout).WithPolling(pollInterval).Should(Succeed())
		})

		It("7. installs e2e_probe with the OdooUpgrade "+installUpgrade, func() {
			if !s.addonSynced {
				Skip("the addon was not synced in step 6")
			}
			By("creating the install OdooUpgrade")
			Expect(utils.ApplyManifest(installManifest())).To(Succeed())
			waitForPhase("odooupgrade", installUpgrade, phaseSucceeded, phaseFailed, upgradeTimeout)

			By("checking the module state in the database")
			Eventually(func(g Gomega) {
				out, err := utils.Output(kubectlNS("exec", "deploy/"+pgName, "--", "psql",
					"-U", pgUser, "-d", pgDatabase, "-tAc",
					"select state from ir_module_module where name='"+addonModule+"'"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("installed"))
			}).WithTimeout(webTimeout).WithPolling(pollInterval).Should(Succeed())
		})

		It("8. gates pod rollouts on spec.upgrade", func() {
			By("setting spec.upgrade for " + addonModule)
			_, err := utils.Output(kubectlNS("patch", "odooinstance", instanceName, "--type=merge",
				"-p", `{"spec":{"upgrade":{"modules":["`+addonModule+`"]}}}`))
			Expect(err).NotTo(HaveOccurred())

			By("checking that the instance stays running")
			waitForValue("odooinstance", instanceName, "{.status.phase}", phaseRunning, webTimeout)
			waitForValue("odooinstance", instanceName, "{.status.ready}", valueTrueLower, webTimeout)
		})

		It("9. writes a PostgreSQL dump and a filestore archive to a PVC", func() {
			By("creating the backup PVC and the OdooBackup " + backupName)
			Expect(utils.ApplyManifest(backupManifest())).To(Succeed())
			waitForPhase("odoobackup", backupName, phaseSucceeded, phaseFailed, backupTimeout)

			By("checking status.lastBackup")
			lastBackup, err := getField("odoobackup", backupName, "{.status.lastBackup}")
			Expect(err).NotTo(HaveOccurred())
			Expect(lastBackup).To(ContainSubstring(".dump"))

			By("listing the backup PVC from a read-only pod")
			out, err := runCheckPod(checkPod{
				Name:   "e2e-backup-list",
				Image:  busyboxImage,
				Script: "ls -1 /backup",
				Claim:  backupPVCName,
			}, podTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`(?m)^e2e-.+\.dump$`), "no dump file in:\n%s", out)
			if !strings.Contains(out, "filestore.tar.gz") {
				// Expected for a fresh database: Odoo creates the filestore directory on first attachment.
				_, _ = fmt.Fprintf(GinkgoWriter, "no filestore archive in %s (the filestore may be empty)\n", out)
			}
		})

		It("10. removes the addon files when the OdooAddon is deleted", func() {
			if !s.addonSynced {
				Skip("the addon was not synced in step 6")
			}
			By("deleting OdooAddon " + addonName)
			_, err := utils.Output(kubectlNS("delete", "odooaddon", addonName, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the finalizer to finish and remove the object")
			Eventually(func(g Gomega) {
				name, err := getField("odooaddon", addonName, "{.metadata.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(name).To(BeEmpty(), "OdooAddon is still present: the finalizer did not finish")
			}).WithTimeout(addonTimeout).WithPolling(pollInterval).Should(Succeed())

			By("checking that the symlink is gone from the web pod")
			Eventually(func(g Gomega) {
				out, err := listAddonsDir()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(ContainSubstring(addonModule+" -> "), "symlink still present:\n%s", out)
			}).WithTimeout(addonTimeout).WithPolling(pollInterval).Should(Succeed())
			s.addonSynced = false
		})

		It("11. keeps the data PVC when the OdooInstance is deleted", func() {
			By("deleting OdooInstance " + instanceName)
			_, err := utils.Output(kubectlNS("delete", "odooinstance", instanceName,
				"--wait=true", "--timeout="+deleteTimeout.String()))
			Expect(err).NotTo(HaveOccurred())

			By("checking that " + dataPVCName + " is still there (retainOnDelete defaults to true)")
			name, err := getField("pvc", dataPVCName, "{.metadata.name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(name).To(Equal(dataPVCName))
		})
	})
}

// createSecrets creates the three secrets of the scenario, each with a random password.
func createSecrets() error {
	return utils.ApplyManifest(fmt.Sprintf(secretsManifest, namespace,
		pgDBSecret, randomPassword(),
		pgAdminSecret, randomPassword(),
		loginSecret, randomPassword()))
}

// loginCheckPod logs in through the web Service with the generated password and with the default one.
// The generated password comes from the loginSecret, through the pod environment.
func loginCheckPod() checkPod {
	return checkPod{
		Name:   "e2e-login-check",
		Image:  curlImage,
		Script: loginCheckScript(),
		Env: []corev1.EnvVar{{
			Name: "GOOD_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: loginSecret},
				Key:                  "password",
			}},
		}},
	}
}

// cronPodIPs returns the IP addresses of the cron pods.
func cronPodIPs() []string {
	out, err := utils.Output(kubectlNS("get", "pods", "-l", "app=odoo-cron",
		"-o", "jsonpath={.items[*].status.podIP}"))
	Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// serviceIPs returns the addresses of the web Service endpoints.
func serviceIPs() []string {
	out, err := utils.Output(kubectlNS("get", "endpointslices", "-l", "kubernetes.io/service-name="+instanceName,
		"-o", "jsonpath={.items[*].endpoints[*].addresses[*]}"))
	Expect(err).NotTo(HaveOccurred())
	Expect(out).NotTo(BeEmpty(), "the web Service has no endpoints")
	return strings.Fields(out)
}

// listAddonsDir returns the output of ls -l on the addons volume of the web pod.
func listAddonsDir() (string, error) {
	return utils.Output(kubectlNS("exec", "deploy/"+instanceName, "--", "ls", "-l", addonsDir))
}

// kubectlNS builds a kubectl command in the scenario namespace. The namespace flag goes right after
// the verb: appended at the end it would land after "--" in "kubectl exec" and become part of the
// command that runs in the container.
func kubectlNS(args ...string) *exec.Cmd {
	if len(args) == 0 {
		return exec.Command(kubectlBin, "-n", namespace)
	}
	full := append([]string{args[0], "-n", namespace}, args[1:]...)
	return exec.Command(kubectlBin, full...)
}

// getField reads one jsonpath of an object. A missing object gives an empty value and no error.
func getField(kind, name, jsonpath string) (string, error) {
	return utils.Output(kubectlNS("get", kind, name, "--ignore-not-found", "-o", "jsonpath="+jsonpath))
}

// waitForValue polls one jsonpath of an object until it equals want.
func waitForValue(kind, name, jsonpath, want string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		got, err := getField(kind, name, jsonpath)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(want), "%s/%s %s", kind, name, jsonpath)
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(Succeed())
}

// waitForPhase polls status.phase. The wait stops at once when the object reaches the failed phase,
// because those phases are final for upgrades and backups. Pass an empty failed phase to keep waiting.
func waitForPhase(kind, name, want, failed string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		phase, err := getField(kind, name, "{.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		if failed != "" && phase == failed {
			StopTrying(fmt.Sprintf("%s/%s reached phase %s", kind, name, failed)).Now()
		}
		g.Expect(phase).To(Equal(want), "%s/%s", kind, name)
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(Succeed())
}

// randomPassword returns 20 random alphanumeric characters.
func randomPassword() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	for i, b := range raw {
		raw[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(raw)
}

// checkPod describes a short-lived pod that runs one shell script in the scenario namespace.
// It uses the restricted Pod Security profile of the namespace.
type checkPod struct {
	Name   string
	Image  string
	Script string
	Env    []corev1.EnvVar
	// Claim, when set, is mounted read-only at /backup.
	Claim string
}

// checkPodManifest renders a restricted-profile pod as JSON.
func checkPodManifest(p checkPod) ([]byte, error) {
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolPtr(true),
				RunAsUser:      int64Ptr(100),
				RunAsGroup:     int64Ptr(100),
				FSGroup:        int64Ptr(100),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "check",
				Image:   p.Image,
				Command: []string{"/bin/sh", "-c", p.Script},
				Env:     p.Env,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             boolPtr(true),
					AllowPrivilegeEscalation: boolPtr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if p.Claim != "" {
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "backup",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: p.Claim,
				ReadOnly:  true,
			}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name: "backup", MountPath: "/backup", ReadOnly: true,
		}}
	}
	return json.Marshal(pod)
}

// runCheckPod runs the pod to completion and returns its log. The pod is deleted afterwards.
func runCheckPod(p checkPod, timeout time.Duration) (string, error) {
	manifest, err := checkPodManifest(p)
	if err != nil {
		return "", err
	}
	deletePod(p.Name)
	defer deletePod(p.Name)

	create := kubectlNS("create", "-f", "-")
	create.Stdin = bytes.NewReader(manifest)
	if _, err := utils.Output(create); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var phase string
	for {
		phase, err = getField("pod", p.Name, "{.status.phase}")
		if err != nil {
			return "", err
		}
		if phase == podPhaseDone || phase == podPhaseFailed {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("pod %s still in phase %q after %s", p.Name, phase, timeout)
		}
		time.Sleep(pollInterval)
	}

	logs, err := utils.Output(kubectlNS("logs", p.Name))
	if err != nil {
		return logs, err
	}
	if phase == podPhaseFailed {
		return logs, fmt.Errorf("pod %s failed:\n%s", p.Name, logs)
	}
	return logs, nil
}

// deletePod removes a check pod if it exists.
func deletePod(name string) {
	_, _ = utils.Output(kubectlNS("delete", "pod", name, "--ignore-not-found", "--wait=true", "--timeout=60s"))
}

// parseKeyValueLines turns "KEY=value" lines into a map. Values may contain "=".
func parseKeyValueLines(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			fields[key] = value
		}
	}
	return fields
}

// isLoggedIn reports whether a JSON-RPC answer to /web/session/authenticate carries a numeric uid.
// A failed login answers with a null or false result, or with an error, and yields false.
func isLoggedIn(payload string) bool {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return false
	}
	var result struct {
		UID *int `json:"uid"`
	}
	return json.Unmarshal(envelope.Result, &result) == nil && result.UID != nil
}

// loginCheckScript prints LOGIN_HTTP, AUTH_GOOD (generated password) and AUTH_DEFAULT (password "admin").
func loginCheckScript() string {
	return fmt.Sprintf(`base="http://%[1]s.%[2]s.svc.cluster.local:8069"
db="%[3]s"
authenticate() {
  curl -sS --max-time 30 -H 'Content-Type: application/json' -d "$1" "$base/web/session/authenticate"
}
echo "LOGIN_HTTP=$(curl -sS --max-time 30 -o /dev/null -w '%%{http_code}' "$base/web/login")"
body='{"jsonrpc":"2.0","method":"call","id":1,"params":{"db":"%%s","login":"admin","password":"%%s"}}'
good=$(printf "$body" "$db" "$GOOD_PASSWORD")
echo "AUTH_GOOD=$(authenticate "$good")"
bad=$(printf '{"jsonrpc":"2.0","method":"call","id":2,"params":{"db":"%%s","login":"admin","password":"admin"}}' "$db")
echo "AUTH_DEFAULT=$(authenticate "$bad")"
`, instanceName, namespace, pgDatabase)
}

// cleanupStack deletes the scenario objects. The addon goes first, while the operator still runs,
// so its finalizer can clean the addons volume.
func cleanupStack() {
	targets := []string{
		"odooaddon/" + addonName,
		"odoobackup/" + backupName,
		"odooupgrade/" + installUpgrade,
		"odooupgrade/" + initUpgrade,
		"odooinstance/" + instanceName,
		"deployment/" + pgName,
		"service/" + pgName,
		"pvc/" + dataPVCName,
		"pvc/" + backupPVCName,
		"secret/" + pgDBSecret,
		"secret/" + pgAdminSecret,
		"secret/" + loginSecret,
	}
	for _, target := range targets {
		_, _ = utils.Output(kubectlNS("delete", target, "--ignore-not-found", "--wait=true", "--timeout=3m"))
	}
}

// dumpDiagnostics writes the state of the scenario namespace to GinkgoWriter.
// Secrets are not dumped.
func dumpDiagnostics() {
	dump := func(title string, args ...string) {
		out, err := utils.Output(kubectlNS(args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "\n----- %s (error: %v) -----\n%s\n", title, err, out)
	}
	dump("pods, jobs, deployments, services, PVCs", "get",
		"pods,jobs,deployments,services,pvc,endpointslices", "-o", "wide")
	dump("events", "get", "events", "--sort-by=.lastTimestamp")
	dump("OdooInstance, OdooAddon, OdooUpgrade, OdooBackup", "get",
		"odooinstances,odooaddons,odooupgrades,odoobackups", "-o", "yaml")
	dump("ConfigMap "+configMapName, "get", "configmap", configMapName, "-o", "yaml")
	dump("describe pods, jobs, deployments, PVCs", "describe", "pods,jobs,deployments,pvc")
	dump("operator logs", "logs", "-l", "control-plane=controller-manager", "--all-containers=true", "--tail=2000")

	names, err := utils.Output(kubectlNS("get", "pods", "-o", "name"))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "cannot list pods: %v\n", err)
		return
	}
	for _, pod := range utils.GetNonEmptyLines(names) {
		dump("logs of "+pod, "logs", pod, "--all-containers=true", "--prefix=true", "--tail=400")
	}
}

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool { return &v }

// int64Ptr returns a pointer to v.
func int64Ptr(v int64) *int64 { return &v }

// secretsManifest holds the three secrets. Arguments: namespace, then name and password pairs.
const secretsManifest = `apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
type: Opaque
stringData:
  password: "%[3]s"
---
apiVersion: v1
kind: Secret
metadata:
  name: %[4]s
  namespace: %[1]s
type: Opaque
stringData:
  password: "%[5]s"
---
apiVersion: v1
kind: Secret
metadata:
  name: %[6]s
  namespace: %[1]s
type: Opaque
stringData:
  password: "%[7]s"
`

// postgresManifest runs a single PostgreSQL 17 pod with TLS. The init container creates a
// self-signed certificate; libpq uses sslmode=require, which does not verify it.
func postgresManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[1]s
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 999
        runAsGroup: 999
        fsGroup: 999
        seccompProfile:
          type: RuntimeDefault
      initContainers:
      - name: tls
        image: %[3]s
        command: ["/bin/sh", "-c"]
        args:
        - >-
          openssl req -x509 -nodes -newkey rsa:2048 -days 1 -subj /CN=postgres
          -keyout /certs/tls.key -out /certs/tls.crt && chmod 600 /certs/tls.key
        securityContext:
          runAsNonRoot: true
          runAsUser: 999
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: certs
          mountPath: /certs
      containers:
      - name: postgres
        image: %[3]s
        args:
        - -c
        - ssl=on
        - -c
        - ssl_cert_file=/certs/tls.crt
        - -c
        - ssl_key_file=/certs/tls.key
        env:
        - name: POSTGRES_USER
          value: %[4]s
        - name: POSTGRES_DB
          value: %[5]s
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: %[6]s
              key: password
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: %[6]s
              key: password
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        ports:
        - containerPort: 5432
          name: postgres
        readinessProbe:
          exec:
            command: ["pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", "%[4]s", "-d", "%[5]s"]
          periodSeconds: 5
        securityContext:
          runAsNonRoot: true
          runAsUser: 999
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
        - name: certs
          mountPath: /certs
      volumes:
      - name: data
        emptyDir: {}
      - name: certs
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app: %[1]s
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
`, pgName, namespace, pgImage, pgUser, pgDatabase, pgDBSecret)
}

// instanceManifest is the OdooInstance of the scenario, initialised on creation.
func instanceManifest() string {
	return fmt.Sprintf(`apiVersion: odoo.operator.io/v1
kind: OdooInstance
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  image: %[3]s
  replicas: 1
  postgres:
    host: %[4]s.%[2]s.svc.cluster.local
    port: 5432
    database: %[5]s
    user: %[6]s
    passwordSecret: %[7]s
  adminPasswordSecret: %[8]s
  addonsVolume:
    size: 1Gi
    accessMode: ReadWriteOnce
  dataVolume:
    size: 2Gi
    accessMode: ReadWriteOnce
    retainOnDelete: true
  config:
    workers: "0"
  resources:
    requests:
      cpu: 250m
      memory: 512Mi
    limits:
      cpu: "2"
      memory: 2Gi
  initialize:
    modules: ["base"]
    adminUserPasswordSecret: %[9]s
`, instanceName, namespace, odooImage, pgName, pgDatabase, pgUser, pgDBSecret, pgAdminSecret, loginSecret)
}

// addonManifest syncs the e2e-addons folder of this repository at the given commit.
func addonManifest(sha string) string {
	return fmt.Sprintf(`apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  gitUrl: https://github.com/acarmisc/odoo-k8-operator.git
  gitRef: %[3]s
  addonPath: %[4]s
  instanceRef:
    name: %[5]s
`, addonName, namespace, sha, addonSourceDir, instanceName)
}

// installManifest installs the probe module into the database.
func installManifest() string {
	return fmt.Sprintf(`apiVersion: odoo.operator.io/v1
kind: OdooUpgrade
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  instanceRef:
    name: %[3]s
  installModules: ["%[4]s"]
  timeoutSeconds: 900
`, installUpgrade, namespace, instanceName, addonModule)
}

// backupManifest makes a one-shot backup to the backup PVC, which it creates first.
func backupManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: odoo.operator.io/v1
kind: OdooBackup
metadata:
  name: %[3]s
  namespace: %[2]s
spec:
  instanceRef:
    name: %[4]s
  includeFilestore: true
  retention: 2
  destination:
    pvc:
      claimName: %[1]s
`, backupPVCName, namespace, backupName, instanceName)
}
