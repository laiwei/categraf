package containers

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExtractContainerID(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "docker scope",
			line: "0::/system.slice/docker-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope",
			want: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "k8s containerd path",
			line: "0::/kubepods.slice/kubepods-besteffort.slice/cri-containerd-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899.scope",
			want: "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899",
		},
		{
			name: "short docker id",
			line: "0::/docker/0123456789ab",
			want: "0123456789ab",
		},
		{
			name: "invalid",
			line: "0::/user.slice/user-1000.slice/session-1.scope",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContainerID(tt.line)
			if got != tt.want {
				t.Fatalf("extractContainerID()=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestApplyInspectMetadata(t *testing.T) {
	r := &Registry{}
	c := NewContainer("abc123")

	r.applyInspectMetadata(c, container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name: "/demo-nginx",
		},
		Config: &container.Config{
			Image: "nginx:1.27",
			Labels: map[string]string{
				"io.kubernetes.pod.name":      "demo-pod",
				"io.kubernetes.pod.namespace": "default",
				"app":                         "demo",
			},
		},
	})

	if c.Name != "demo-nginx" {
		t.Fatalf("unexpected container name: %s", c.Name)
	}
	if c.Image != "nginx:1.27" {
		t.Fatalf("unexpected image: %s", c.Image)
	}
	if c.PodName != "demo-pod" {
		t.Fatalf("unexpected pod name: %s", c.PodName)
	}
	if c.Namespace != "default" {
		t.Fatalf("unexpected namespace: %s", c.Namespace)
	}
	if c.Labels["app"] != "demo" {
		t.Fatalf("unexpected label app: %s", c.Labels["app"])
	}
}

func TestNormalizeContainerID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "docker://012345", want: "012345"},
		{in: "containerd://abcdef.scope", want: "abcdef"},
		{in: " plain-id ", want: "plain-id"},
		{in: "", want: ""},
	}

	for _, tt := range tests {
		got := normalizeContainerID(tt.in)
		if got != tt.want {
			t.Fatalf("normalizeContainerID(%q)=%q want=%q", tt.in, got, tt.want)
		}
	}
}

func TestIndexPodContainerMeta(t *testing.T) {
	meta := map[string]k8sContainerMeta{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app": "nginx",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			},
		},
	}

	indexPodContainerMeta(meta, pod)

	v, ok := meta["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
	if !ok {
		t.Fatal("container id not indexed")
	}
	if v.PodName != "nginx-pod" || v.Namespace != "default" {
		t.Fatalf("unexpected pod meta: %+v", v)
	}
	if v.Labels["app"] != "nginx" {
		t.Fatalf("unexpected labels: %+v", v.Labels)
	}
}
