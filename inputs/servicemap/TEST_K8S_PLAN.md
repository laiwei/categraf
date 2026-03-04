# 场景二测试：Kubernetes 无侵入可观测性

> **目标**：在 Ubuntu 虚拟机上用 Minikube 搭建单节点 K8s，验证 servicemap 能自动发现 Pod 拓扑、关联 K8s 元数据、解析 L7 协议。

---

## 一、环境准备

**前置条件**

| 项目 | 要求 |
|------|------|
| OS   | Ubuntu 20.04+ |
| 内核 | ≥ 5.4（需支持 BTF：`ls /sys/kernel/btf/vmlinux`） |
| CPU / 内存 | ≥ 2C / 4G |
| 软件 | Docker 20.10+、Go 1.21+、git |

```bash
# 安装 Docker（如未安装）
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER && newgrp docker
```

---

## 二、搭建 Minikube

```bash
# 安装 minikube
curl -LO https://github.com/kubernetes/minikube/releases/latest/download/minikube-linux-amd64
sudo install minikube-linux-amd64 /usr/local/bin/minikube

# 安装 kubectl
curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl

# 启动集群（daocloud 镜像源，集群名 sm-test）
minikube start --driver=docker \
  --image-mirror-country=cn \
  --registry-mirror=https://docker.m.daocloud.io \
  -p sm-test

kubectl get nodes   # 验证 Ready
```

---

## 三、编译 categraf

```bash
# 安装 eBPF 编译依赖
sudo apt-get install -y clang llvm bpftool libbpf-dev make git golang

# 克隆代码（替换为你的实际仓库地址）
git clone <your-repo-url> ~/categraf && cd ~/categraf

# 编译 eBPF 程序
cd inputs/servicemap/tracer/bpf
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
cd .. && make clean && make

# 编译 categraf 二进制
cd ~/categraf && go build -o categraf .
```

---

## 四、部署测试应用

最简拓扑：`curl-client` → `nginx`（HTTP GET 循环）

```bash
kubectl create namespace demo

kubectl -n demo apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
spec:
  replicas: 1
  selector:
    matchLabels: {app: nginx}
  template:
    metadata:
      labels: {app: nginx}
    spec:
      containers:
        - name: nginx
          image: docker.m.daocloud.io/nginx:alpine
---
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  selector: {app: nginx}
  ports:
    - port: 80
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: curl-client
spec:
  replicas: 1
  selector:
    matchLabels: {app: curl-client}
  template:
    metadata:
      labels: {app: curl-client}
    spec:
      containers:
        - name: curl
          image: docker.m.daocloud.io/curlimages/curl:latest
          command:
            - sh
            - -c
            - "while true; do curl -s http://nginx/ > /dev/null; sleep 3; done"
EOF

# 等待 Pod 就绪（约 30s）
kubectl -n demo get pods -w
```

---

## 五、部署 categraf servicemap

### 5.1 构建镜像（直接注入 Minikube）

```bash
# 将 Docker CLI 指向 Minikube 内部 daemon，build 的镜像无需额外 load
eval $(minikube docker-env -p sm-test)

cd ~/categraf
cat > Dockerfile.sm <<'EOF'
FROM docker.m.daocloud.io/ubuntu:22.04
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY categraf /usr/local/bin/categraf
ENTRYPOINT ["/usr/local/bin/categraf"]
EOF

docker build -f Dockerfile.sm -t categraf-sm:test .
docker images | grep categraf-sm   # 确认镜像存在
```

### 5.2 部署 RBAC + ConfigMap + DaemonSet

```bash
kubectl create namespace monitoring

kubectl apply -f - <<'MANIFEST'
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: categraf-sm
  namespace: monitoring
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: categraf-sm
rules:
  - apiGroups: [""]
    resources: [pods, nodes]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: categraf-sm
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: categraf-sm
subjects:
  - kind: ServiceAccount
    name: categraf-sm
    namespace: monitoring
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: sm-config
  namespace: monitoring
data:
  config.toml: |
    [global]
    hostname = "$HOSTNAME"
    interval = 15
  servicemap.toml: |
    [[instances]]
    enable_tcp    = true
    enable_http   = true
    enable_k8s    = true
    enable_cgroup = true
    api_addr      = ":9099"
    ignore_ports  = [22, 9100, 10250, 10255]
    ignore_cidrs  = ["127.0.0.0/8"]
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: categraf-sm
  namespace: monitoring
spec:
  selector:
    matchLabels: {app: categraf-sm}
  template:
    metadata:
      labels: {app: categraf-sm}
    spec:
      serviceAccountName: categraf-sm
      hostNetwork: true
      hostPID: true
      tolerations:
        - operator: Exists
      containers:
        - name: categraf
          image: categraf-sm:test
          imagePullPolicy: Never
          securityContext:
            privileged: true
          env:
            - name: CATEGRAF_CONFIGS
              value: /etc/categraf/conf
          volumeMounts:
            - name: cgroup
              mountPath: /sys/fs/cgroup
              readOnly: true
            - name: proc
              mountPath: /proc
              readOnly: true
            - name: main-cfg
              mountPath: /etc/categraf/conf/config.toml
              subPath: config.toml
            - name: plugin-cfg
              mountPath: /etc/categraf/conf/input.servicemap/servicemap.toml
              subPath: servicemap.toml
          ports:
            - containerPort: 9099
              hostPort: 9099
      volumes:
        - name: cgroup
          hostPath: {path: /sys/fs/cgroup}
        - name: proc
          hostPath: {path: /proc}
        - name: main-cfg
          configMap:
            name: sm-config
            items:
              - key: config.toml
                path: config.toml
        - name: plugin-cfg
          configMap:
            name: sm-config
            items:
              - key: servicemap.toml
                path: servicemap.toml
MANIFEST

# 等待 Pod 就绪（1/1 Running）
kubectl -n monitoring get pods -w
```

---

## 六、验证

```bash
# 端口转发（另开终端保持运行）
POD=$(kubectl -n monitoring get pods -l app=categraf-sm -o jsonpath='{.items[0].metadata.name}')
kubectl -n monitoring port-forward $POD 9099:9099 &

# 等待首个采集周期
sleep 30
```

### 6.1 查看文本拓扑

```bash
curl -s http://localhost:9099/graph/text
```

**期望输出示例**：
```
=== Service Map @ ... ===
Nodes (2):
  [xxx] name=nginx       ns=demo  pod=nginx-xxxxx
  [yyy] name=curl-client ns=demo  pod=curl-client-xxxxx
Edges (1):
  yyy -> <nginx-pod-ip>:80  [TCP, HTTP]
    TCP: connects=N ...
    HTTP GET 200(2xx): req=N ...
```

### 6.2 逐项检查

```bash
DATA=$(curl -s http://localhost:9099/graph)

# V1：K8s 元数据关联（namespace + pod_name）
echo "=== V1: K8s 元数据 ==="
echo $DATA | jq '.nodes[] | {name, namespace, pod_name}'

# V2：规模
echo "=== V2: 规模 ==="
echo $DATA | jq '.summary'

# V3：TCP 边数
echo "=== V3: TCP 边 ==="
echo $DATA | jq '[.edges[] | select(.tcp != null)] | length'

# V4：HTTP L7 解析
echo "=== V4: HTTP 边 ==="
echo $DATA | jq '[.edges[] | select(.http != null and (.http|length>0))] | length'

# V5：eBPF 模式确认
echo "=== V5: eBPF 模式 ==="
kubectl -n monitoring logs $POD | grep -i "ebpf programs loaded" || echo "未找到，请检查日志"
```

### 6.3 通过标准

| # | 验证项 | 通过条件 |
|---|--------|---------|
| **V1** | K8s 元数据 | namespace=demo 节点数 ≥ 2（nginx、curl-client 各含 pod_name） |
| **V2** | 规模 | `nodes ≥ 2, edges ≥ 1` |
| **V3** | TCP | TCP 边数 ≥ 1 |
| **V4** | HTTP L7 | HTTP 边数 ≥ 1（curl-client → nginx 的 GET） |
| **V5** | eBPF | 日志中出现 `"eBPF programs loaded"`，不出现 `"polling fallback"` |

---

## 七、清理

```bash
kubectl delete ns demo monitoring
kubectl delete clusterrole,clusterrolebinding categraf-sm
minikube delete -p sm-test
```

---

## 常见问题

| 现象 | 原因 | 解决 |
|------|------|------|
| Pod `CrashLoopBackOff` | eBPF 加载失败或权限不足 | `kubectl -n monitoring logs $POD` 查看具体错误 |
| 节点无 `namespace` 字段 | K8s 客户端未初始化 | 检查 RBAC，确认日志无 `"init kubernetes client failed"` |
| 无 HTTP 边 | L7 解析未生效 | 确认内核 ≥ 5.4，检查 `disable_l7_tracing` 未被设为 true |
| 镜像拉取失败 (`ErrImageNeverPull`) | 未执行 `eval $(minikube docker-env)` | 重新执行后再 `docker build` |
| Graph API 返回 nodes=0 | 采集周期未到 | 等待约 30s（两个采集周期）再查询 |
