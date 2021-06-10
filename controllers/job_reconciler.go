/*
Copyright 2021 The Scribe authors.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type JobReconciler struct {
	logger  logr.Logger
	scheme  *runtime.Scheme
	owner   metav1.Object
	job     *batchv1.Job
	desired corev1.PodTemplateSpec
	paused  bool
}

func NewJobReconciler(
	logger logr.Logger,
	scheme *runtime.Scheme,
	owner metav1.Object,
	job *batchv1.Job) JobReconciler {
	return JobReconciler{
		logger: logger,
		scheme: scheme,
		owner:  owner,
		job:    job,
	}
}

func (jr *JobReconciler) SetPause(pause bool) {
	jr.paused = pause
}

func (jr *JobReconciler) SetServiceAccount(saName string) {
	jr.desired.Spec.ServiceAccountName = saName
}

// Reconcile updates the object in the API server to match the desired state.
func (jr *JobReconciler) Reconcile(ctx context.Context, cl client.Client) (bool, error) {
	op, err := ctrlutil.CreateOrUpdate(ctx, cl, jr.job, jr.Apply)
	// XXX Should do something about cases where we're trying to change an immutable field in the Job
	if err != nil {
		jr.logger.Error(err, "job reconcile failed")
		return false, err
	}
	if jr.job.Status.Failed >= *jr.job.Spec.BackoffLimit {
		jr.logger.Info("deleting job -- backoff limit exceeded")
		err = cl.Delete(ctx, jr.job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		return false, err
	}

	jr.logger.Info("job reconciled", "operation", op)

	return true, nil
}

// Apply is a mutation function compatible w/ CreateOrUpdate.
var _ ctrlutil.MutateFn = (&JobReconciler{}).Apply

// Apply adjusts the Job to match the desired state.
func (jr *JobReconciler) Apply() error {
	if err := ctrl.SetControllerReference(jr.owner, jr.job, jr.scheme); err != nil {
		jr.logger.Error(err, "unable to set controller reference")
		return err
	}

	// Merge labels
	if jr.job.Spec.Template.Labels == nil {
		jr.job.Spec.Template.Labels = map[string]string{}
	}
	for k, v := range jr.desired.Labels {
		jr.job.Spec.Template.Labels[k] = v
	}

	backoffLimit := int32(2)
	jr.job.Spec.BackoffLimit = &backoffLimit

	jr.job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever

	parallelism := int32(1)
	if jr.paused {
		parallelism = 0
	}
	jr.job.Spec.Parallelism = &parallelism

	jr.job.Spec.Template.Spec.Containers = jr.desired.Spec.Containers
	jr.job.Spec.Template.Spec.ServiceAccountName = jr.desired.Spec.ServiceAccountName
	jr.job.Spec.Template.Spec.Volumes = jr.desired.Spec.Volumes

	return nil
}

func (jr *JobReconciler) ConfigureRclone(
	isSource bool,
	dataPVCName string,
	destinationPath string,
	configSection string,
	rcloneSecretName string) {
	runAsUser := int64(0)
	secretMode := int32(0600)

	direction := "destination"
	if isSource {
		direction = "source"
	}

	jr.desired.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "rclone",
			Env: []corev1.EnvVar{
				{Name: "RCLONE_DEST_PATH", Value: destinationPath},
				{Name: "DIRECTION", Value: direction},
				{Name: "RCLONE_CONFIG", Value: "/rclone-config/rclone.conf"},
				{Name: "RCLONE_CONFIG_SECTION", Value: configSection},
				{Name: "MOUNT_PATH", Value: mountPath},
			},
			Command: []string{"/bin/bash", "-c", "./active.sh"},
			Image:   RcloneContainerImage,
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: &runAsUser,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataVolumeName, MountPath: mountPath},
				{Name: rcloneSecret, MountPath: "/rclone-config"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName,
				}},
			},
			{Name: rcloneSecret, VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  rcloneSecretName,
					DefaultMode: &secretMode,
				}},
			},
		},
	}
}

type ResticAction string

const (
	BackupOnly     ResticAction = "BackupOnly"
	BackupAndPrune ResticAction = "BackupAndPrune"
	Restore        ResticAction = "Restore"
)

//nolint:funlen
func (jr *JobReconciler) ConfigureRestic(
	action ResticAction,
	forgetOptions string,
	dataPVCName string,
	cachePVCName string,
	resticSecretName string) {
	runAsUser := int64(0)

	actions := []string{"backup"}
	if action == BackupAndPrune {
		actions = []string{"backup", "prune"}
	}
	if action == Restore {
		actions = []string{"restore"}
	}

	jr.desired.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "restic",
			Env: []corev1.EnvVar{
				{Name: "FORGET_OPTIONS", Value: forgetOptions},
				{Name: "DATA_DIR", Value: mountPath},
				{Name: "RESTIC_CACHE_DIR", Value: resticCacheMountPath},
				// We populate environment variables from the restic repo
				// Secret. They are taken 1-for-1 from the Secret into env vars.
				// The allowed variables are defined by restic.
				// https://restic.readthedocs.io/en/stable/040_backup.html#environment-variables
				// Mandatory variables are needed to define the repository
				// location and its password.
				envFromSecret(resticSecretName, "RESTIC_REPOSITORY", false),
				envFromSecret(resticSecretName, "RESTIC_PASSWORD", false),

				// Optional variables based on what backend is used for restic
				envFromSecret(resticSecretName, "AWS_ACCESS_KEY_ID", true),
				envFromSecret(resticSecretName, "AWS_SECRET_ACCESS_KEY", true),
				envFromSecret(resticSecretName, "AWS_DEFAULT_REGION", true),

				envFromSecret(resticSecretName, "ST_AUTH", true),
				envFromSecret(resticSecretName, "ST_USER", true),
				envFromSecret(resticSecretName, "ST_KEY", true),

				envFromSecret(resticSecretName, "OS_AUTH_URL", true),
				envFromSecret(resticSecretName, "OS_REGION_NAME", true),
				envFromSecret(resticSecretName, "OS_USERNAME", true),
				envFromSecret(resticSecretName, "OS_USER_ID", true),
				envFromSecret(resticSecretName, "OS_PASSWORD", true),
				envFromSecret(resticSecretName, "OS_TENANT_ID", true),
				envFromSecret(resticSecretName, "OS_TENANT_NAME", true),

				envFromSecret(resticSecretName, "OS_USER_DOMAIN_NAME", true),
				envFromSecret(resticSecretName, "OS_USER_DOMAIN_ID", true),
				envFromSecret(resticSecretName, "OS_PROJECT_NAME", true),
				envFromSecret(resticSecretName, "OS_PROJECT_DOMAIN_NAME", true),
				envFromSecret(resticSecretName, "OS_PROJECT_DOMAIN_ID", true),
				envFromSecret(resticSecretName, "OS_TRUST_ID", true),

				envFromSecret(resticSecretName, "OS_APPLICATION_CREDENTIAL_ID", true),
				envFromSecret(resticSecretName, "OS_APPLICATION_CREDENTIAL_NAME", true),
				envFromSecret(resticSecretName, "OS_APPLICATION_CREDENTIAL_SECRET", true),

				envFromSecret(resticSecretName, "OS_STORAGE_URL", true),
				envFromSecret(resticSecretName, "OS_AUTH_TOKEN", true),

				envFromSecret(resticSecretName, "B2_ACCOUNT_ID", true),
				envFromSecret(resticSecretName, "B2_ACCOUNT_KEY", true),

				envFromSecret(resticSecretName, "AZURE_ACCOUNT_NAME", true),
				envFromSecret(resticSecretName, "AZURE_ACCOUNT_KEY", true),

				envFromSecret(resticSecretName, "GOOGLE_PROJECT_ID", true),
				envFromSecret(resticSecretName, "GOOGLE_APPLICATION_CREDENTIALS", true),
			},
			Command: []string{"/entry.sh"},
			Args:    actions,
			Image:   ResticContainerImage,
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: &runAsUser,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataVolumeName, MountPath: mountPath},
				{Name: resticCache, MountPath: resticCacheMountPath},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName,
				}},
			},
			{Name: resticCache, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: cachePVCName,
				}},
			},
		},
	}
}

func (jr *JobReconciler) ConfigureRsync(
	labels map[string]string,
	isSource bool,
	dataPVCName string,
	sshSecretName string) {
	runAsUser := int64(0)
	secretMode := int32(0600)

	command := []string{"/bin/bash", "-c", "/destination.sh"}
	if isSource {
		command = []string{"/bin/bash", "-c", "/source.sh"}
	}

	jr.desired.Labels = labels
	jr.desired.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:    "rsync",
			Command: command,
			Image:   RsyncContainerImage,
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{
						"AUDIT_WRITE",
						"SYS_CHROOT",
					},
				},
				RunAsUser: &runAsUser,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataVolumeName, MountPath: mountPath},
				{Name: "keys", MountPath: "/keys"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName,
				}},
			},
			{Name: "keys", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  sshSecretName,
					DefaultMode: &secretMode,
				}},
			},
		},
	}
}

// AddEnv adds an environment variable to the container. The container structure
// must have already been populated via a Configure* call.
func (jr *JobReconciler) AddEnv(name string, value string) {
	// Currently all the movers are single-container pods, but assuming that
	// this will always be true is fragile.
	jr.desired.Spec.Containers[0].Env = append(jr.desired.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  name,
		Value: value,
	})
}
