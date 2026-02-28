package servicemap

import (
	"testing"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/containers"
)

func TestBuild_EmptyContainers(t *testing.T) {
	g := Build(nil)
	if len(g.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(g.Nodes))
	}
	if len(g.Edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(g.Edges))
	}
}

func TestBuild_SingleContainer(t *testing.T) {
	c := containers.NewContainer("test-id")
	c.Name = "test-container"
	c.Namespace = "default"
	c.PodName = "test-pod"

	stats := &containers.TCPStats{
		SuccessfulConnects: 10,
		BytesSent:          1024,
	}
	c.TCPStats["10.0.0.1:80"] = stats

	g := Build([]*containers.Container{c})
	if len(g.Nodes) != 1 {
		t.Errorf("expected 1 node, got %d", len(g.Nodes))
	}
	if len(g.Edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(g.Edges))
	}

	node, ok := g.Nodes["test-id"]
	if !ok {
		t.Fatal("node not found")
	}
	if node.Name != "test-container" {
		t.Errorf("expected name test-container, got %s", node.Name)
	}

	edgeKey := "test-id->10.0.0.1:80"
	edge, ok := g.Edges[edgeKey]
	if !ok {
		t.Fatal("edge not found")
	}
	if edge.Source.ID != "test-id" {
		t.Errorf("expected source ID test-id, got %s", edge.Source.ID)
	}
	if edge.DestHost != "10.0.0.1" {
		t.Errorf("expected dest host 10.0.0.1, got %s", edge.DestHost)
	}
	if edge.DestPort != "80" {
		t.Errorf("expected dest port 80, got %s", edge.DestPort)
	}
	if edge.SuccessfulConnects != 10 {
		t.Errorf("expected 10 successful connects, got %d", edge.SuccessfulConnects)
	}
	if edge.BytesSent != 1024 {
		t.Errorf("expected 1024 bytes sent, got %d", edge.BytesSent)
	}
}

func TestSplitEndpoint(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantPort string
	}{
		{"10.0.0.1:80", "10.0.0.1", "80"},
		{"192.168.1.1:443", "192.168.1.1", "443"},
		{"[::1]:8080", "::1", "8080"},
		{"localhost:3000", "localhost", "3000"},
		{"", "", ""},
		{"invalid", "invalid", ""},
		{"10.0.0.1:port", "10.0.0.1", "port"},  // net.SplitHostPort 能解析非数字端口
	}

	for _, tc := range tests {
		h, p := splitEndpoint(tc.input)
		if h != tc.wantHost || p != tc.wantPort {
			t.Errorf("splitEndpoint(%q) = (%q, %q), want (%q, %q)",
				tc.input, h, p, tc.wantHost, tc.wantPort)
		}
	}
}

func TestSourceNode_DefaultValues(t *testing.T) {
	c := containers.NewContainer("")
	c.Name = ""

	node := sourceNode(c)
	if node.ID != "unknown" {
		t.Errorf("expected ID unknown, got %s", node.ID)
	}
	if node.Name != "unknown" {
		t.Errorf("expected Name unknown, got %s", node.Name)
	}
}

func TestBuild_NilContainer(t *testing.T) {
	cs := []*containers.Container{nil}
	g := Build(cs)
	if len(g.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(g.Nodes))
	}
	if len(g.Edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(g.Edges))
	}
}

func TestBuild_AggregateStats(t *testing.T) {
	c := containers.NewContainer("container1")
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{
		SuccessfulConnects: 5,
		BytesSent:          100,
	}

	g := Build([]*containers.Container{c})

	edgeKey := "container1->10.0.0.1:80"
	edge := g.Edges[edgeKey]
	if edge.SuccessfulConnects != 5 {
		t.Errorf("expected 5, got %d", edge.SuccessfulConnects)
	}
	if edge.BytesSent != 100 {
		t.Errorf("expected 100, got %d", edge.BytesSent)
	}
}
