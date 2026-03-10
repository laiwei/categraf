package graph

import (
	"net"
	"strconv"
	"strings"

	"flashcat.cloud/categraf/inputs/servicemap/containers"
)

type Node struct {
	ID        string
	Name      string
	Namespace string
	PodName   string
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

		// P0-3: 使用快照方法避免并发读写竞争
		tcpStats := c.GetTCPStatsSnapshot()
		listenEndpoints := c.GetListenEndpointsSnapshot()

		// 无出站连接且无监听端口的容器为真正的孤立节点，不加入拓扑图。
		// 有监听端口但无出站连接的纯服务端（nc/redis/nginx/sshd 等）
		// 应作为 server-only 节点出现，使跨主机拓扑 JOIN 时能正确连线。
		if len(tcpStats) == 0 && len(listenEndpoints) == 0 {
			continue
		}

		src := sourceNode(c)
		g.Nodes[src.ID] = src

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
		ID:        id,
		Name:      name,
		Namespace: c.Namespace,
		PodName:   c.PodName,
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
