package snrutils

import (
	"context"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestVerifyAgentConfiguration(t *testing.T) {
	spec := map[string]interface{}{"peerUpdateInterval": "17m", "maxApiErrorThreshold": int64(5)}
	podSpec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Env: []corev1.EnvVar{
		{Name: "PEER_UPDATE_INTERVAL", Value: "1020000000000"},
		{Name: "MAX_API_ERROR_THRESHOLD", Value: "5"},
	}}}}
	if err := VerifyAgentConfiguration(podSpec, spec, "run-token"); err != nil {
		t.Fatal(err)
	}
	stale := podSpec.DeepCopy()
	stale.Containers[0].Env[0].Value = "900000000000"
	if err := VerifyAgentConfiguration(stale, spec, "run-token"); err == nil {
		t.Fatal("accepted stale agent config")
	}
	probe := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	probe = probe.DeepCopy()
	_ = unstructured.SetNestedSlice(probe.Object, []interface{}{map[string]interface{}{
		"key": helpers.FBCRunLabel, "value": "run-token", "operator": "Equal", "effect": "NoSchedule",
	}}, "spec", "customDsTolerations")
	probeSpec, _, _ := unstructured.NestedMap(probe.Object, "spec")
	if err := VerifyAgentConfiguration(podSpec, probeSpec, "run-token"); err == nil {
		t.Fatal("accepted a probe that never reached the agents")
	}
	podSpec.Tolerations = []corev1.Toleration{{
		Key: helpers.FBCRunLabel, Value: "run-token",
		Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule,
	}}
	if err := VerifyAgentConfiguration(podSpec, probeSpec, "run-token"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentConfiguration(podSpec, spec, "run-token"); err == nil {
		t.Fatal("accepted an agent template with the unrestored probe")
	}
}

func TestVerifyConfiguration(t *testing.T) {
	spec := map[string]interface{}{"peerUpdateInterval": "17m", "maxApiErrorThreshold": int64(5)}
	object := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	object.SetUID("original-uid")

	for _, test := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		wantErr bool
	}{
		{name: "unchanged"},
		{name: "status only", change: func(object *unstructured.Unstructured) {
			object.Object["status"] = map[string]interface{}{"ready": false}
		}},
		{name: "recreated", wantErr: true, change: func(object *unstructured.Unstructured) {
			object.SetUID("replacement-uid")
		}},
		{name: "changed field", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "18m", "spec", "peerUpdateInterval")
		}},
		{name: "added field", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, int64(30002), "spec", "hostPort")
		}},
		{name: "missing spec", wantErr: true, change: func(object *unstructured.Unstructured) {
			delete(object.Object, "spec")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := object.DeepCopy()
			if test.change != nil {
				test.change(changed)
			}
			err := helpers.VerifyConfigurationPreserved(context.Background(), nil, changed, object.GetUID(), spec)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected preservation result: %v", err)
			}
		})
	}
	if err := helpers.VerifyConfigurationPreserved(context.Background(), nil, object, "", spec); err == nil {
		t.Fatal("accepted an absent baseline identity")
	}
}

func TestVerifyAgents(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 1,
			UpdatedNumberScheduled: 1, NumberReady: 1,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", UID: "new-uid"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "expected-image"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	for _, test := range []struct {
		name    string
		change  func(*appsv1.DaemonSet, *corev1.Pod)
		wantErr bool
	}{
		{name: "ready rollout"},
		{name: "zero agents", wantErr: true, change: func(ds *appsv1.DaemonSet, _ *corev1.Pod) {
			ds.Status.DesiredNumberScheduled = 0
		}},
		{name: "stale generation", wantErr: true, change: func(ds *appsv1.DaemonSet, _ *corev1.Pod) {
			ds.Status.ObservedGeneration = 1
		}},
		{name: "old pod", wantErr: true, change: func(_ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.UID = "old-uid"
		}},
		{name: "wrong image", wantErr: true, change: func(_ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.Spec.Containers[0].Image = "wrong-image"
		}},
		{name: "unready pod", wantErr: true, change: func(_ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.Status.Conditions = nil
		}},
		{name: "terminating pod", wantErr: true, change: func(_ *appsv1.DaemonSet, pod *corev1.Pod) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ds, changedPod := daemonSet.DeepCopy(), pod.DeepCopy()
			if test.change != nil {
				test.change(ds, changedPod)
			}
			err := VerifyAgents(ds, []corev1.Pod{*changedPod}, "expected-image", map[types.UID]bool{"old-uid": true})
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected agent readiness result: %v", err)
			}
		})
	}
	if err := VerifyAgents(daemonSet, nil, "expected-image", nil); err == nil {
		t.Fatal("accepted missing agent pods")
	}
}
