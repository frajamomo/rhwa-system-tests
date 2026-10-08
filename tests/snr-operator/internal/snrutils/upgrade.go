package snrutils

import (
	"fmt"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// VerifyAgents requires a fully observed rollout and all current pods Ready.
// oldUIDs optionally requires actual pod replacement after a config probe.
func VerifyAgents(
	daemonSet *appsv1.DaemonSet, pods []corev1.Pod, expectedImage string, oldUIDs map[types.UID]bool,
) error {
	desired := daemonSet.Status.DesiredNumberScheduled
	if desired == 0 || daemonSet.Status.ObservedGeneration < daemonSet.Generation ||
		daemonSet.Status.UpdatedNumberScheduled != desired || daemonSet.Status.NumberReady != desired ||
		daemonSet.Status.NumberUnavailable != 0 || int32(len(pods)) != desired {
		return fmt.Errorf("SNR agent DaemonSet rollout is incomplete: generation=%d status=%+v pods=%d",
			daemonSet.Generation, daemonSet.Status, len(pods))
	}

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || oldUIDs[pod.UID] {
			return fmt.Errorf("agent %s is terminating, not Running, or still has its pre-probe UID", pod.Name)
		}
		ready := false

		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || !helpers.PodContainerUsesImage(&pod, snrparams.ManagerContainerName, expectedImage) {
			return fmt.Errorf("agent %s is not Ready or is not running %s", pod.Name, expectedImage)
		}
	}

	return nil
}
