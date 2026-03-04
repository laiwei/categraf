package servicemap

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────
// /graph/view — 交互式可视化拓扑页面（Cytoscape.js）
// ─────────────────────────────────────────────────────────────

func (ins *Instance) handleGraphView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = fmt.Fprint(w, graphViewHTML)
}

const graphViewHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Service Map — Topology View</title>
<script src="https://unpkg.com/cytoscape@3.30.4/dist/cytoscape.min.js"></script>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0d1117; color: #c9d1d9; }
  #toolbar { display: flex; align-items: center; gap: 12px; padding: 8px 16px; background: #161b22; border-bottom: 1px solid #30363d; flex-wrap: wrap; }
  #toolbar label { font-size: 13px; color: #8b949e; }
  #toolbar select, #toolbar input, #toolbar button { font-size: 13px; padding: 4px 8px; background: #21262d; color: #c9d1d9; border: 1px solid #30363d; border-radius: 4px; }
  #toolbar button { cursor: pointer; }
  #toolbar button:hover { background: #30363d; }
  #toolbar .sep { width: 1px; height: 20px; background: #30363d; }
  #summary { font-size: 13px; color: #8b949e; margin-left: auto; }
  #cy { width: 100%; height: calc(100vh - 44px); }
  #tooltip { position: absolute; display: none; background: #1c2128; border: 1px solid #30363d; border-radius: 6px; padding: 10px 14px; font-size: 12px; line-height: 1.6; color: #c9d1d9; max-width: 420px; pointer-events: none; z-index: 1000; box-shadow: 0 4px 12px rgba(0,0,0,.4); }
  #tooltip .tt-title { font-weight: 600; font-size: 13px; margin-bottom: 4px; color: #58a6ff; }
  #tooltip .tt-row { color: #8b949e; }
  #tooltip .tt-val { color: #c9d1d9; }
</style>
</head>
<body>
<div id="toolbar">
  <label>布局</label>
  <select id="layout">
    <option value="cose">力导向</option>
    <option value="breadthfirst">层次</option>
    <option value="circle">环形</option>
    <option value="grid">网格</option>
  </select>
  <div class="sep"></div>
  <label>过滤</label>
  <input id="filter" type="text" placeholder="关键词…" style="width:140px">
  <div class="sep"></div>
  <button id="btn-refresh" title="刷新数据">⟳ 刷新</button>
  <label><input type="checkbox" id="auto-refresh"> 自动 <select id="refresh-interval"><option value="5">5s</option><option value="10" selected>10s</option><option value="30">30s</option><option value="60">60s</option></select></label>
  <span id="summary"></span>
</div>
<div id="cy"></div>
<div id="tooltip"></div>

<script>
(function() {
  const layoutSel = document.getElementById('layout');
  const filterInput = document.getElementById('filter');
  const btnRefresh = document.getElementById('btn-refresh');
  const autoRefreshCB = document.getElementById('auto-refresh');
  const refreshIntervalSel = document.getElementById('refresh-interval');
  const summaryEl = document.getElementById('summary');
  const tooltipEl = document.getElementById('tooltip');

  let cy, autoTimer = null;

  // — 初始化 Cytoscape ——————————————————————————
  function initCy() {
    cy = cytoscape({
      container: document.getElementById('cy'),
      elements: [],
      style: [
        { selector: 'node.process', style: {
          'label': 'data(label)', 'text-valign': 'bottom', 'text-margin-y': 6,
          'font-size': 11, 'color': '#c9d1d9', 'text-outline-width': 2, 'text-outline-color': '#0d1117',
          'background-color': '#1f6feb', 'border-width': 2, 'border-color': '#388bfd',
          'width': 36, 'height': 36, 'shape': 'ellipse',
        }},
        { selector: 'node.container', style: {
          'label': 'data(label)', 'text-valign': 'bottom', 'text-margin-y': 6,
          'font-size': 11, 'color': '#c9d1d9', 'text-outline-width': 2, 'text-outline-color': '#0d1117',
          'background-color': '#238636', 'border-width': 2, 'border-color': '#2ea043',
          'width': 36, 'height': 36, 'shape': 'round-rectangle',
        }},
        { selector: 'node.external', style: {
          'label': 'data(label)', 'text-valign': 'bottom', 'text-margin-y': 6,
          'font-size': 10, 'color': '#8b949e', 'text-outline-width': 2, 'text-outline-color': '#0d1117',
          'background-color': '#30363d', 'border-width': 1, 'border-color': '#484f58',
          'width': 28, 'height': 28, 'shape': 'diamond',
        }},
        { selector: 'node:selected', style: { 'border-color': '#f0883e', 'border-width': 3 }},
        { selector: 'edge', style: {
          'width': 'data(weight)', 'line-color': '#30363d', 'target-arrow-color': '#30363d',
          'target-arrow-shape': 'triangle', 'curve-style': 'bezier', 'arrow-scale': 0.8,
          'label': 'data(label)', 'font-size': 9, 'color': '#6e7681',
          'text-outline-width': 1.5, 'text-outline-color': '#0d1117', 'text-rotation': 'autorotate',
        }},
        { selector: 'edge.highlighted', style: { 'line-color': '#58a6ff', 'target-arrow-color': '#58a6ff', 'z-index': 10 }},
        { selector: 'node.dimmed', style: { 'opacity': 0.15 }},
        { selector: 'edge.dimmed', style: { 'opacity': 0.08 }},
      ],
      minZoom: 0.2, maxZoom: 5,
      wheelSensitivity: 0.3,
    });

    // — 点击高亮邻居 —
    cy.on('tap', 'node', function(evt) {
      const node = evt.target;
      const neighborhood = node.closedNeighborhood();
      cy.elements().addClass('dimmed');
      neighborhood.removeClass('dimmed');
      neighborhood.edges().addClass('highlighted');
    });
    cy.on('tap', function(evt) {
      if (evt.target === cy) { cy.elements().removeClass('dimmed highlighted'); }
    });

    // — Tooltip —
    cy.on('mouseover', 'node', function(evt) { showTooltip(evt, nodeTooltip(evt.target.data())); });
    cy.on('mouseover', 'edge', function(evt) { showTooltip(evt, edgeTooltip(evt.target.data())); });
    cy.on('mouseout', function() { tooltipEl.style.display = 'none'; });
    cy.on('drag', function() { tooltipEl.style.display = 'none'; });
  }

  function showTooltip(evt, html) {
    tooltipEl.innerHTML = html;
    tooltipEl.style.display = 'block';
    const rect = document.getElementById('cy').getBoundingClientRect();
    const x = evt.renderedPosition.x + rect.left + 14;
    const y = evt.renderedPosition.y + rect.top + 14;
    tooltipEl.style.left = Math.min(x, window.innerWidth - 440) + 'px';
    tooltipEl.style.top = Math.min(y, window.innerHeight - 200) + 'px';
  }

  function nodeTooltip(d) {
    let h = '<div class="tt-title">' + esc(d.label) + '</div>';
    h += '<div class="tt-row">ID: <span class="tt-val">' + esc(d.id) + '</span></div>';
    if (d.nodeType) h += '<div class="tt-row">类型: <span class="tt-val">' + esc(d.nodeType) + '</span></div>';
    if (d.ns) h += '<div class="tt-row">Namespace: <span class="tt-val">' + esc(d.ns) + '</span></div>';
    if (d.pod) h += '<div class="tt-row">Pod: <span class="tt-val">' + esc(d.pod) + '</span></div>';
    if (d.image) h += '<div class="tt-row">Image: <span class="tt-val">' + esc(d.image) + '</span></div>';
    return h;
  }

  function edgeTooltip(d) {
    let h = '<div class="tt-title">' + esc(d.source) + ' → ' + esc(d.target) + '</div>';
    if (d.protocols) h += '<div class="tt-row">协议: <span class="tt-val">' + esc(d.protocols) + '</span></div>';
    if (d.tcp) {
      const t = d.tcp;
      h += '<div class="tt-row">连接: <span class="tt-val">' + t.connects + '</span> (失败 ' + t.failed + ', 活跃 ' + t.active + ')</div>';
      h += '<div class="tt-row">重传: <span class="tt-val">' + t.retx + '</span></div>';
      if (t.sent > 0 || t.recv > 0) h += '<div class="tt-row">流量: <span class="tt-val">↑' + fmtBytes(t.sent) + ' ↓' + fmtBytes(t.recv) + '</span></div>';
      if (t.avgMs > 0) h += '<div class="tt-row">平均耗时: <span class="tt-val">' + t.avgMs.toFixed(2) + 'ms</span></div>';
    }
    return h;
  }

  function fmtBytes(b) {
    if (b < 1024) return b + 'B';
    if (b < 1048576) return (b/1024).toFixed(1) + 'KB';
    if (b < 1073741824) return (b/1048576).toFixed(1) + 'MB';
    return (b/1073741824).toFixed(2) + 'GB';
  }

  function esc(s) { const d = document.createElement('div'); d.textContent = s||''; return d.innerHTML; }

  // — 数据加载 ————————————————————————————————
  async function loadData() {
    const kw = filterInput.value.trim();
    let url = '/graph?edges_only=true';
    if (kw) url += '&filter=' + encodeURIComponent(kw);
    try {
      const resp = await fetch(url);
      const data = await resp.json();
      renderGraph(data);
    } catch(e) {
      console.error('fetch /graph failed:', e);
    }
  }

  function renderGraph(data) {
    const elements = [];
    const nodeIds = new Set();

    // 源节点（进程/容器）
    for (const n of (data.nodes||[])) {
      const cls = n.id.startsWith('proc_') ? 'process' : 'container';
      nodeIds.add(n.id);
      elements.push({ group: 'nodes', data: {
        id: n.id, label: n.name || n.id, nodeType: cls === 'process' ? '裸进程' : '容器',
        ns: n.namespace, pod: n.pod_name, image: n.image,
      }, classes: cls });
    }

    // 外部目标节点 + 边
    for (const e of (data.edges||[])) {
      if (!nodeIds.has(e.target)) {
        nodeIds.add(e.target);
        // 美化标签：只显示端口或短地址
        let extLabel = e.target;
        if (e.target_port) extLabel = ':' + e.target_port;
        elements.push({ group: 'nodes', data: {
          id: e.target, label: extLabel, nodeType: '外部端点',
        }, classes: 'external' });
      }
      const w = Math.max(1.5, Math.min(6, Math.log2((e.tcp?.connects_total||1) + 1)));
      let edgeLabel = '';
      if (e.tcp) edgeLabel = (e.tcp.active_connections > 0 ? '●' + e.tcp.active_connections : '');
      elements.push({ group: 'edges', data: {
        id: e.id, source: e.source, target: e.target, weight: w, label: edgeLabel,
        protocols: (e.protocols||[]).join(', '),
        tcp: e.tcp ? {
          connects: e.tcp.connects_total||0, failed: e.tcp.connect_failed_total||0,
          active: e.tcp.active_connections||0, retx: e.tcp.retransmits_total||0,
          sent: e.tcp.bytes_sent_total||0, recv: e.tcp.bytes_received_total||0,
          avgMs: e.tcp.avg_connect_duration_ms||0,
        } : null,
      }});
    }

    // 更新元素（保留位置优化：如果节点集没变则只更新数据）
    cy.elements().remove();
    cy.add(elements);
    runLayout();

    summaryEl.textContent = data.summary
      ? data.summary.nodes + ' 节点, ' + data.summary.edges + ' 边 | 活跃连接 ' + data.summary.tracer_active_connections
      : '';
  }

  function runLayout() {
    cy.layout({
      name: layoutSel.value,
      animate: true, animationDuration: 400,
      // cose 参数
      nodeRepulsion: function() { return 8000; },
      idealEdgeLength: function() { return 120; },
      edgeElasticity: function() { return 100; },
      gravity: 0.25,
      numIter: 200,
      // breadthfirst 参数
      directed: true, spacingFactor: 1.5,
    }).run();
  }

  // — 事件绑定 ————————————————————————————————
  layoutSel.addEventListener('change', runLayout);
  btnRefresh.addEventListener('click', loadData);
  filterInput.addEventListener('keydown', function(e) { if (e.key === 'Enter') loadData(); });
  autoRefreshCB.addEventListener('change', toggleAutoRefresh);
  refreshIntervalSel.addEventListener('change', toggleAutoRefresh);

  function toggleAutoRefresh() {
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (autoRefreshCB.checked) {
      const sec = parseInt(refreshIntervalSel.value) || 10;
      autoTimer = setInterval(loadData, sec * 1000);
    }
  }

  // — 启动 —
  initCy();
  loadData();
})();
</script>
</body>
</html>`

// ─────────────────────────────────────────────────────────────
// ASCII 拓扑图（追加到 /graph/text 输出末尾）
// ─────────────────────────────────────────────────────────────

// writeASCIITopology 将拓扑关系渲染为简洁的 ASCII 有向图。
// 按源节点分组，每个源节点下列出它的所有连接目标。
//
// 输出示例：
//
//	Topology:
//	  ┌──────────┐      ┌──────────────────────┐
//	  │ categraf │─TCP──▶│ 10.0.0.5:3306        │
//	  └──────────┘      └──────────────────────┘
//	                ├TCP──▶ 10.0.0.6:6379
//	  ┌──────────┐
//	  │ nginx    │─TCP──▶ 10.0.0.7:8080
//	  └──────────┘
func writeASCIITopology(w io.Writer, g GraphResponse) {
	if len(g.Edges) == 0 {
		return
	}

	// 按 Source 分组
	type edgeInfo struct {
		target    string
		protocols string
		active    uint64
		connects  uint64
	}
	groups := make(map[string][]edgeInfo)
	var sources []string

	for _, e := range g.Edges {
		proto := strings.Join(e.Protocols, ",")
		var active, connects uint64
		if e.TCP != nil {
			active = e.TCP.ActiveConnections
			connects = e.TCP.ConnectsTotal
		}
		if _, exists := groups[e.Source]; !exists {
			sources = append(sources, e.Source)
		}
		groups[e.Source] = append(groups[e.Source], edgeInfo{
			target: e.Target, protocols: proto, active: active, connects: connects,
		})
	}
	sort.Strings(sources)

	// 节点名映射
	nameOf := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.Name != "" {
			nameOf[n.ID] = n.Name
		} else {
			nameOf[n.ID] = n.ID
		}
	}

	var sb strings.Builder
	sb.WriteString("Topology:\n")

	for _, src := range sources {
		edges := groups[src]
		name := nameOf[src]
		if name == "" {
			name = src
		}

		// 源节点方框（中文等宽字符占 2 列，ASCII 占 1 列）
		displayW := 0
		for _, r := range name {
			if r > 0x7F {
				displayW += 2 // CJK / fullwidth
			} else {
				displayW += 1
			}
		}
		boxW := displayW + 2 // 左右各留 1 空格
		top := "┌" + strings.Repeat("─", boxW) + "┐"
		mid := "│ " + name + " │"
		bot := "└" + strings.Repeat("─", boxW) + "┘"

		for i, e := range edges {
			// 箭头标签
			label := e.protocols
			if e.active > 0 {
				label += fmt.Sprintf(" ●%d", e.active)
			} else if e.connects > 0 {
				label += fmt.Sprintf(" ×%d", e.connects)
			}

			arrow := fmt.Sprintf("─%s─▶ %s", label, e.target)

			if i == 0 {
				// 第一条边：画完整方框
				pad := strings.Repeat(" ", boxW+2)
				sb.WriteString(fmt.Sprintf("  %s\n", top))
				sb.WriteString(fmt.Sprintf("  %s%s\n", mid, arrow))
				sb.WriteString(fmt.Sprintf("  %s\n", bot))
				_ = pad
			} else {
				// 后续边：续接线（对齐到方框中部）
				indent := strings.Repeat(" ", displayW/2+2)
				sb.WriteString(fmt.Sprintf("  %s├%s\n", indent, arrow))
			}
		}
	}

	_, _ = fmt.Fprint(w, sb.String())
}
