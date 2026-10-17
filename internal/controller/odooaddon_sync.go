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

// Pure (cluster-free) helpers for the OdooAddon sync: input validation, the shell
// script that runs inside the sync Job, and the Job spec itself. Everything here is
// deterministic so it can be unit tested without an API server.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	// defaultSyncImage is used unless OPERATOR_SYNC_IMAGE overrides it (see syncImage).
	defaultSyncImage = "alpine/git:2.47.2"
	syncImageEnvVar  = "OPERATOR_SYNC_IMAGE"

	defaultGitRef  = "main"
	defaultGitUser = "x-access-token"

	syncJobSuffix     = "-sync"
	syncContainerName = "sync"
	syncBackoffLimit  = int32(1)
	syncTTLSeconds    = int32(300)
	// syncActiveDeadlineSeconds bounds a hung clone at the Job level (JobSpec, not PodSpec).
	syncActiveDeadlineSeconds = int64(180)
	// syncUID is the non-root uid/gid the sync container runs as. fsGroup gives it
	// write access to the addons PVC.
	syncUID = int64(65532)

	syncRepoDir     = "/tmp/repo"
	syncReleasesDir = ".releases"
	syncManifest    = "__manifest__.py"

	gitSecretMountPath  = "/etc/git-secret"
	gitSecretVolume     = "git-secret"
	gitSecretSSHKeyFile = "ssh-privatekey"
	gitSecretKnownHosts = "known_hosts"
	gitSecretTokenKey   = "token"
	gitSecretUserKey    = "username"
	// gitSSHSecretMode is group-readable on purpose: Secret files
	// are root-owned and the sync pod runs as 65532, so it reads the key through fsGroup.
	gitSSHSecretMode = int32(0o440)

	envGitURL        = "GIT_URL"
	envGitRef        = "GIT_REF"
	envGitToken      = "GIT_TOKEN"
	envGitUsername   = "GIT_USERNAME"
	envGitSSHCommand = "GIT_SSH_COMMAND"
	envHome          = "HOME"

	volumeScratch = "scratch"
	scratchMount  = "/tmp"

	labelAddonOwner    = "odoo.operator.io/addon"
	schemeHTTPS        = "https://"
	schemeHTTP         = "http://"
	schemeSSH          = "ssh://"
	scpStylePrefix     = "git@"
	credentialsErrText = "gitUrl must not embed credentials; put them in gitSecretRef instead"
)

const (
	// annotationSyncSpecHash records syncSpecHash on the Job so a reconcile can tell a
	// Job built from the current spec from one built from an earlier spec.
	annotationSyncSpecHash = "odoo.operator.io/sync-spec-hash"
	syncSpecHashLen        = 16
)

// gitAuthMode says how the sync Job authenticates to the git remote.
type gitAuthMode int

const (
	authNone gitAuthMode = iota
	// authHTTPSToken sends an Authorization header built from the secret's token.
	authHTTPSToken
	// authSSH mounts the secret's SSH key and known_hosts and points GIT_SSH_COMMAND at them.
	authSSH
)

var validGitRef = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
var validPathSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var shSafe = regexp.MustCompile(`^[a-zA-Z0-9._/+-]+$`)

// syncImage returns the sync image, overridable through OPERATOR_SYNC_IMAGE.
func syncImage() string {
	if v := os.Getenv(syncImageEnvVar); v != "" {
		return v
	}
	return defaultSyncImage
}

// syncJobImage is resolved once at start-up from OPERATOR_SYNC_IMAGE.
var syncJobImage = syncImage()

// syncSpec is an OdooAddon spec after defaulting and validation, ready for script/Job generation.
type syncSpec struct {
	GitURL      string
	GitRef      string
	AddonName   string
	ModuleName  string
	AddonPath   string
	SingleAddon bool
	// SecretName is the gitSecretRef name, empty when no credentials are configured.
	SecretName string
	Auth       gitAuthMode
}

// syncSpecHash fingerprints every input that changes what the sync Job produces. It is
// stored on the Job so a Job built from an older spec is never mistaken for the current one.
func syncSpecHash(s syncSpec) string {
	// json.Marshal of a slice is unambiguous (every field is quoted and escaped), so
	// values that contain separators cannot make two different specs hash the same.
	fields, _ := json.Marshal([]string{
		s.GitURL, s.GitRef, s.AddonPath, s.ModuleName, fmt.Sprint(s.SingleAddon), s.SecretName, s.AddonName,
	})
	sum := sha256.Sum256(fields)
	return hex.EncodeToString(sum[:])[:syncSpecHashLen]
}

// isCommitSHA reports whether ref is a full 40-character lowercase hex commit id.
func isCommitSHA(ref string) bool {
	return commitSHA.MatchString(ref)
}

// canSkipResync reports whether a periodic resync can be skipped: the addon is pinned
// to a commit SHA that is already synced, so nothing can have changed upstream.
func canSkipResync(addon *odoov1.OdooAddon) bool {
	ref := addon.Spec.GitRef
	if ref == "" {
		ref = defaultGitRef
	}
	return isCommitSHA(ref) &&
		addon.Status.Phase == phaseSynced &&
		addon.Status.ClonedCommit == ref
}

// validateGitUrl checks the scheme and rejects credentials embedded in the URL.
// Error messages never echo the URL, since it may contain a password.
func validateGitUrl(raw string) error {
	if !strings.HasPrefix(raw, schemeHTTPS) && !strings.HasPrefix(raw, schemeHTTP) &&
		!strings.HasPrefix(raw, schemeSSH) && !strings.HasPrefix(raw, scpStylePrefix) {
		return fmt.Errorf("gitUrl scheme not allowed")
	}
	if strings.HasPrefix(raw, scpStylePrefix) {
		// scp-style "git@host:path" carries a user name only, never a password.
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gitUrl is not a valid URL")
	}
	if parsed.User == nil {
		return nil
	}
	// ssh://user@host is normal; any other userinfo (token-as-username included) is not.
	_, hasPassword := parsed.User.Password()
	if !strings.HasPrefix(raw, schemeSSH) || hasPassword {
		return fmt.Errorf("%s", credentialsErrText)
	}
	return nil
}

func validateGitRef(ref string) error {
	if strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid gitRef: %q", ref)
	}
	if !validGitRef.MatchString(ref) {
		return fmt.Errorf("gitRef contains disallowed characters: %q", ref)
	}
	return nil
}

// validatePathSegment guards moduleName/addonPath, which get interpolated into the
// sync Job's shell script - reject anything but a plain relative path/name so a
// malicious OdooAddon spec can't break out of /addons or inject shell commands.
func validatePathSegment(name string) error {
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid path segment: %q", name)
	}
	if !validPathSegment.MatchString(name) {
		return fmt.Errorf("path segment contains disallowed characters: %q", name)
	}
	return nil
}

// validateModuleName additionally forbids a leading dot: dot-directories under the addons
// mount are reserved for the release store (.releases) and the switch temp links (.<m>.tmp).
func validateModuleName(name string) error {
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("moduleName must not start with '.': %q", name)
	}
	return validatePathSegment(name)
}

// resolveGitAuth picks the auth mode from the URL scheme and whether a secret is set.
func resolveGitAuth(gitURL string, secret *odoov1.LocalSecretRef) (gitAuthMode, error) {
	if secret == nil {
		return authNone, nil
	}
	if secret.Name == "" {
		return authNone, fmt.Errorf("gitSecretRef.name must not be empty")
	}
	if strings.HasPrefix(gitURL, schemeSSH) || strings.HasPrefix(gitURL, scpStylePrefix) {
		return authSSH, nil
	}
	if strings.HasPrefix(gitURL, schemeHTTP) {
		return authNone, fmt.Errorf("refusing to send git credentials over plain http; use https")
	}
	return authHTTPSToken, nil
}

// resolveSyncSpec applies defaults and runs every validation for an OdooAddon sync.
func resolveSyncSpec(addon *odoov1.OdooAddon) (syncSpec, error) {
	s := syncSpec{
		GitURL:      addon.Spec.GitUrl,
		GitRef:      addon.Spec.GitRef,
		AddonName:   addon.Name,
		ModuleName:  addon.Spec.ModuleName,
		AddonPath:   addon.Spec.AddonPath,
		SingleAddon: addon.Spec.SingleAddon,
	}
	if s.GitRef == "" {
		s.GitRef = defaultGitRef
	}
	if s.ModuleName == "" {
		s.ModuleName = addon.Name
	}

	if err := validateGitUrl(s.GitURL); err != nil {
		return syncSpec{}, err
	}
	if err := validateGitRef(s.GitRef); err != nil {
		return syncSpec{}, err
	}
	if err := validatePathSegment(s.AddonName); err != nil {
		return syncSpec{}, fmt.Errorf("invalid addon name: %w", err)
	}
	if err := validateModuleName(s.ModuleName); err != nil {
		return syncSpec{}, fmt.Errorf("invalid moduleName: %w", err)
	}
	if s.AddonPath != "" {
		if err := validatePathSegment(s.AddonPath); err != nil {
			return syncSpec{}, fmt.Errorf("invalid addonPath: %w", err)
		}
	}

	auth, err := resolveGitAuth(s.GitURL, addon.Spec.GitSecretRef)
	if err != nil {
		return syncSpec{}, err
	}
	s.Auth = auth
	if addon.Spec.GitSecretRef != nil {
		s.SecretName = addon.Spec.GitSecretRef.Name
	}
	return s, nil
}

// shQuote returns s safe to embed in a POSIX shell script. Plain paths stay unquoted
// so the generated script stays readable.
func shQuote(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildSyncScript returns the sh script run by the sync Job. Invariants:
//   - the token is only ever referenced as "$GIT_TOKEN" or inside $AUTH, never literally,
//     and `set +x` keeps shell tracing from echoing it;
//   - modules are staged in a release directory and only switched in via symlinks, so
//     Odoo never sees a half-copied module;
//   - the last stdout line is the commit SHA; everything else goes to stderr.
func buildSyncScript(s syncSpec) string {
	gitCmd := "git"
	authSetup := ""
	if s.Auth == authHTTPSToken {
		// The token is sent as an HTTP Basic header via -c, so it never appears in the
		// clone URL, the process arguments or the logs.
		authSetup = "GIT_USERNAME=\"${" + envGitUsername + ":-" + defaultGitUser + "}\"\n" +
			"AUTH=$(printf '%s:%s' \"$GIT_USERNAME\" \"$GIT_TOKEN\" | base64 | tr -d '\\n')\n"
		gitCmd = `git -c "http.extraHeader=Authorization: Basic $AUTH"`
	}

	var fetch string
	if isCommitSHA(s.GitRef) {
		// A commit cannot be cloned with --branch; fetch exactly that object instead.
		fetch = "git init " + syncRepoDir + "\n" +
			"git -C " + syncRepoDir + " remote add origin \"$GIT_URL\"\n" +
			gitCmd + " -C " + syncRepoDir + " fetch --depth 1 origin \"$GIT_REF\"\n" +
			"git -C " + syncRepoDir + " checkout --detach FETCH_HEAD\n"
	} else {
		fetch = gitCmd + " clone --depth 1 --branch \"$GIT_REF\" \"$GIT_URL\" " + syncRepoDir + "\n"
	}

	// publish makes one module live: it copies the module into this commit's release only
	// if the release lacks it (so a re-sync of the same commit with a different module
	// layout still works), then points the module's symlink at it. Each visible change is
	// a single rename, so Odoo never sees a half-copied module.
	const publishFn = `publish() {
  name=$1
  src=$2
  if [ ! -d "$REL/$name" ]; then
    rm -rf "$REL/.$name.partial"
    cp -a "$src" "$REL/.$name.partial"
    rm -rf "$REL/.$name.partial/.git"
    mv "$REL/.$name.partial" "$REL/$name"
  fi
  target="$ADDONS/$name"
  if [ -d "$target" ] && [ ! -L "$target" ]; then
    rm -rf "$target"
  fi
  ln -sfn "$REL/$name" "$ADDONS/.$name.tmp"
  mv -T "$ADDONS/.$name.tmp" "$target"
}
`

	var publishCalls string
	if s.SingleAddon {
		publishCalls = "publish " + shQuote(s.ModuleName) + " " + syncRepoDir + "\n"
	} else {
		src := syncRepoDir
		if s.AddonPath != "" {
			src = syncRepoDir + "/" + s.AddonPath
		}
		publishCalls = "found=0\n" +
			"for mod in " + src + "/*/; do\n" +
			"  if [ -f \"$mod/" + syncManifest + "\" ]; then\n" +
			"    publish \"$(basename \"$mod\")\" \"$mod\"\n" +
			"    found=1\n" +
			"  fi\n" +
			"done\n" +
			"[ \"$found\" = 1 ] || { echo \"no Odoo modules found in " + src + "\" >&2; exit 1; }\n"
	}

	return "set -e\n" +
		"set +x\n" +
		"export GIT_TERMINAL_PROMPT=0\n" +
		authSetup +
		fetch +
		"SHA=$(git -C " + syncRepoDir + " rev-parse HEAD)\n" +
		"ADDONS=" + addonsMountPath + "\n" +
		"RELROOT=" + shQuote(addonsMountPath+"/"+syncReleasesDir+"/"+s.AddonName) + "\n" +
		"REL=\"$RELROOT/$SHA\"\n" +
		"mkdir -p \"$REL\"\n" +
		publishFn +
		publishCalls +
		"touch \"$REL\"\n" +
		"ls -1t \"$RELROOT\" | tail -n +3 | while IFS= read -r old; do rm -rf \"$RELROOT/$old\"; done\n" +
		"git -C " + syncRepoDir + " rev-parse HEAD\n"
}

// syncJobOptions carries the cluster facts buildSyncJob needs but cannot derive itself.
type syncJobOptions struct {
	Image   string
	PVCName string
	// NodeName is the node the instance's Odoo pod runs on, "" if unknown.
	NodeName string
	// AddonsAccessMode is the instance's addonsVolume.accessMode.
	AddonsAccessMode corev1.PersistentVolumeAccessMode
}

// syncJobName returns the name of the sync Job for an addon.
func syncJobName(addonName string) string {
	return addonName + syncJobSuffix
}

// buildSyncJob returns the hardened sync Job for spec. It does not set an owner
// reference; the caller does that with the scheme.
func buildSyncJob(addon *odoov1.OdooAddon, spec syncSpec, opts syncJobOptions) *batchv1.Job {
	env := []corev1.EnvVar{
		{Name: envGitURL, Value: spec.GitURL},
		{Name: envGitRef, Value: spec.GitRef},
		{Name: envHome, Value: scratchMount},
	}
	volumes := []corev1.Volume{
		{
			Name: volumeAddons,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: opts.PVCName},
			},
		},
		{
			Name:         volumeScratch,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: volumeAddons, MountPath: addonsMountPath},
		{Name: volumeScratch, MountPath: scratchMount},
	}

	switch spec.Auth {
	case authHTTPSToken:
		env = append(env,
			corev1.EnvVar{Name: envGitToken, ValueFrom: secretEnvSource(spec.SecretName, gitSecretTokenKey, false)},
			corev1.EnvVar{Name: envGitUsername, ValueFrom: secretEnvSource(spec.SecretName, gitSecretUserKey, true)},
		)
	case authSSH:
		mode := gitSSHSecretMode
		volumes = append(volumes, corev1.Volume{
			Name: gitSecretVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: spec.SecretName, DefaultMode: &mode},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: gitSecretVolume, MountPath: gitSecretMountPath, ReadOnly: true})
		env = append(env, corev1.EnvVar{Name: envGitSSHCommand, Value: sshCommand()})
	}

	podLabels := map[string]string{labelAddonOwner: addon.Name}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncJobName(addon.Name),
			Namespace: addon.Namespace,
			Labels: map[string]string{
				labelManagedBy:  managedByOperator,
				labelAddonOwner: addon.Name,
			},
			Annotations: map[string]string{annotationSyncSpecHash: syncSpecHash(spec)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrTo(syncBackoffLimit),
			TTLSecondsAfterFinished: ptrTo(syncTTLSeconds),
			ActiveDeadlineSeconds:   ptrTo(syncActiveDeadlineSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptrTo(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptrTo(true),
						RunAsUser:    ptrTo(syncUID),
						RunAsGroup:   ptrTo(syncUID),
						FSGroup:      ptrTo(syncUID),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:         syncContainerName,
							Image:        opts.Image,
							Command:      []string{"sh", "-c", buildSyncScript(spec)},
							Env:          env,
							VolumeMounts: mounts,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptrTo(false),
								ReadOnlyRootFilesystem:   ptrTo(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	// The node pin exists only so an RWO addons PVC can be mounted by both the Job and
	// the Odoo pod. An RWX volume has no such restriction, so the Job may schedule anywhere.
	if opts.NodeName != "" && opts.AddonsAccessMode != corev1.ReadWriteMany {
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": opts.NodeName}
	}

	return job
}

func sshCommand() string {
	return "ssh -i " + gitSecretMountPath + "/" + gitSecretSSHKeyFile +
		" -o UserKnownHostsFile=" + gitSecretMountPath + "/" + gitSecretKnownHosts +
		" -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes"
}

func secretEnvSource(name, key string, optional bool) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  key,
			Optional:             ptrTo(optional),
		},
	}
}

// ptrTo returns a pointer to a copy of v.
func ptrTo[T any](v T) *T {
	return &v
}
