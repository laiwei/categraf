package servicemap

import (
	"net"
	"strconv"
	"strings"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/containers"
)

type Node struct {
	ID          string
	Name        string
	Namespace   string
	PodName     string
	ContainerID string
}

type Edge struct {
	Source      Node
	Destination string
	DestHost    string
	DestPort    string

	SuccessfulConnects uint64
	FailedConnects     uint64
	ActiveConnections  uint64
	Retransmissions    uint64
	BytesSent          uint64
	BytesReceived      uint64
}

type Graph struct {
	Nodes map[string]Node
	Edges map[string]*Edge
}

func Build(cs []*containers.Container) Graph {
	g := Graph{
		Nodes: make(map[string]Node),
		Edges: make(map[string]*Edge),
	}

	for _, c := range cs {
		if c == nil {
			continue
		}
		src := sourceNode(c)
		g.Nodes[src.ID] = src

		// P0-3: 使用快照方法避免并发读写竞争
		tcpStats := c.GetTCPStatsSnapshot()
		for dest, s := range tcpStats {
			if s == nil {
				continue
			}
			host, port := splitEndpoint(dest)
			edgeKey := src.ID + "->" + dest
			edge, ok := g.Edges[edgeKey]
			if !ok {
				edge = &Edge{Source: src, Destination: dest, DestHost: host, DestPort: port}
				g.Edges[edgeKey] = edge
			}
			edge.SuccessfulConnects += s.SuccessfulConnects
			edge.FailedConnects += s.FailedConnects
			edge.ActiveConnections += s.ActiveConnections
			edge.Retransmissions += s.Retransmissions
			edge.BytesSent += s.BytesSent
			edge.BytesReceived += s.BytesReceived
		}
	}

	return g
}

func sourceNode(c *containers.Container) Node {
	id := c.ID
	if id == "" {
		id = "unknown"
	}
	name := c.Name
	if name == "" {
		name = id
	}
	return Node{
		ID:          id,
		Name:        name,
		Namespace:   c.Namespace,
		PodName:     c.PodName,
		ContainerID: c.ID,
	}
}

func splitEndpoint(ep string) (string, string) {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return "", ""
	}
	host, port, err := net.SplitHostPort(ep)
	if err == nil {
		return host, port
	}
	if i := strings.LastIndex(ep, ":"); i > 0 && i < len(ep)-1 {
		p := ep[i+1:]
		if _, err := strconv.Atoi(p); err == nil {
			return ep[:i], p
		}
	}
	// 无法解析端口，返回整个字符串作为 host
	return ep, ""
}
