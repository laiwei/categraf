package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// RemoteWriter 将 P2P 拓扑指标写回 Prometheus
type RemoteWriter struct {
	url     string
	timeout time.Duration
	client  *http.Client
}

func NewRemoteWriter(url string, timeout time.Duration) *RemoteWriter {
	return &RemoteWriter{
		url:     url,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// WriteP2PEdges 将 P2P 边列表序列化为 TimeSeries 并通过 Remote Write 写入 Prometheus
func (w *RemoteWriter) WriteP2PEdges(ctx context.Context, edges []P2PEdge) error {
	if len(edges) == 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	var tsList []prompb.TimeSeries

	// 聚合：同一 source_name → dest_source_name 的边求和
	type key struct {
		SourceName, SourceType, DestName, DestType, DestNamespace string
	}
	agg := make(map[key]float64)
	for _, e := range edges {
		k := key{
			SourceName:    e.SourceName,
			SourceType:    e.SourceType,
			DestName:      e.DestName,
			DestType:      e.DestType,
			DestNamespace: e.DestNamespace,
		}
		agg[k] += e.ActiveConnections
	}

	for k, v := range agg {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		ts := prompb.TimeSeries{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "servicemap_p2p_topology_active"},
				{Name: "source_name", Value: k.SourceName},
				{Name: "source_type", Value: k.SourceType},
				{Name: "dest_source_name", Value: k.DestName},
				{Name: "dest_source_type", Value: k.DestType},
				{Name: "dest_namespace", Value: k.DestNamespace},
				{Name: "generated_by", Value: "topology-aggregator"},
			},
			Samples: []prompb.Sample{
				{Value: v, Timestamp: now},
			},
		}
		tsList = append(tsList, ts)
	}

	if len(tsList) == 0 {
		return nil
	}

	req := &prompb.WriteRequest{Timeseries: tsList}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal proto: %w", err)
	}

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := w.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("remote write: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote write returned %d", resp.StatusCode)
	}

	return nil
}
