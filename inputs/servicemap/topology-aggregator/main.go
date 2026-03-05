package topologyaggregator
// Package main: Topology Aggregator Service
//
// 功能：定期从 Prometheus 查询 servicemap_edge_active_connections 和
//       servicemap_listen_endpoint，在内存中 JOIN 出 process/container →
//       process/container 的 P2P 拓扑，支持 K8s Service IP 解析，并通过：
//         1. Remote Write 将结果回写入 Prometheus
//         2. HTTP API /api/v1/topology 暴露 JSON 拓扑图





















































































}	}		}			return			log.Printf("I! topology-aggregator: received signal %v, shutting down", sig)		case sig := <-quit:			agg.Aggregate()		case <-ticker.C:		select {	for {	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)	quit := make(chan os.Signal, 1)	defer ticker.Stop()	ticker := time.NewTicker(cfg.Interval)	agg.Aggregate()	// 立即执行一次，再进入定时循环	}()		}			os.Exit(1)			log.Printf("E! topology-aggregator: API server error: %v", err)		if err := apiServer.Run(); err != nil {	go func() {	apiServer := NewAPIServer(cfg.Listen, agg)	// 启动 HTTP API	agg := NewAggregator(cfg, querier, k8sResolver, writer)	}		log.Printf("I! topology-aggregator: remote write enabled → %s", cfg.RemoteWriteURL)		writer = NewRemoteWriter(cfg.RemoteWriteURL, cfg.QueryTimeout)	if cfg.RemoteWriteURL != "" {	var writer *RemoteWriter	}		}			log.Printf("I! topology-aggregator: K8s resolver initialized")		} else {			log.Printf("W! topology-aggregator: K8s resolver init failed: %v, continuing without K8s resolution", err)		if err != nil {		k8sResolver, err = NewK8sResolver(cfg.KubeConfig)		var err error	if cfg.EnableK8s {	var k8sResolver *K8sResolver	querier := NewPrometheusQuerier(cfg.PrometheusURL, cfg.QueryTimeout)	// 初始化各组件		cfg.PrometheusURL, cfg.Listen, cfg.Interval)	log.Printf("I! topology-aggregator starting, prometheus=%s listen=%s interval=%s",	log.SetFlags(log.LstdFlags | log.Lshortfile)	flag.Parse()	flag.DurationVar(&cfg.QueryTimeout, "query-timeout", 30*time.Second, "Prometheus 查询超时")	flag.StringVar(&cfg.KubeConfig, "kubeconfig", "", "kubeconfig 文件路径（为空则使用 in-cluster 配置）")	flag.BoolVar(&cfg.EnableK8s, "enable-k8s", false, "启用 K8s Service IP 解析（需要 kubeconfig 或 in-cluster 权限）")	flag.DurationVar(&cfg.Interval, "interval", 60*time.Second, "聚合周期")	flag.StringVar(&cfg.Listen, "listen", ":9098", "HTTP API 监听地址")	flag.StringVar(&cfg.RemoteWriteURL, "remote-write-url", "", "Prometheus Remote Write 地址（为空则不写回）")	flag.StringVar(&cfg.PrometheusURL, "prometheus-url", "http://localhost:9090", "Prometheus HTTP API 地址")	cfg := &Config{}func main() {)	"time"	"syscall"	"os/signal"	"os"	"log"	"flag"import (package main//     --remote-write-url=http://prometheus:9090/api/v1/write//     --enable-k8s \//     --interval=60s \//     --listen=:9098 \//     --prometheus-url=http://prometheus:9090 \//   ./topology-aggregator \// 使用：//