package containers

import "testing"

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
