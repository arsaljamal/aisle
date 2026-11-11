"""Generates deploy/grafana/aisle.json (run: python3 scripts/dashboard.py)."""
import json

P = {"type": "prometheus", "uid": "prometheus"}
L = {"type": "loki", "uid": "loki"}
T = {"type": "tempo", "uid": "tempo"}
panels, y = [], 0


def ts(title, exprs, x, w=12, h=8, unit="short", desc=""):
    return {"type": "timeseries", "title": title, "description": desc, "datasource": P,
            "gridPos": {"x": x, "y": y, "w": w, "h": h},
            "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 2, "fillOpacity": 8}}, "overrides": []},
            "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}},
            "targets": [{"datasource": P, "expr": e, "legendFormat": l, "refId": chr(65 + i)} for i, (e, l) in enumerate(exprs)]}


def stat(title, expr, x, unit, thresholds, w=4, desc=""):
    return {"type": "stat", "title": title, "description": desc, "datasource": P,
            "gridPos": {"x": x, "y": y, "w": w, "h": 4},
            "fieldConfig": {"defaults": {"unit": unit, "decimals": 2, "thresholds": {"mode": "absolute", "steps": thresholds}}},
            "options": {"colorMode": "background", "graphMode": "area", "reduceOptions": {"calcs": ["lastNotNull"]}},
            "targets": [{"datasource": P, "expr": expr, "refId": "A"}]}


G, A, R = "green", "orange", "red"
rate = 'sum(rate(aisle_search_duration_seconds_count[1m]))'
panels += [
    stat("Searches / s", rate, 0, "reqps", [{"color": "blue", "value": None}]),
    stat("Error rate", 'sum(rate(aisle_search_errors_total[1m])) / ' + rate + ' or vector(0)', 4, "percentunit",
         [{"color": G, "value": None}, {"color": A, "value": .01}, {"color": R, "value": .05}]),
    stat("p95 latency", 'histogram_quantile(0.95, sum by (le) (rate(aisle_search_duration_seconds_bucket[1m])))', 8, "s",
         [{"color": G, "value": None}, {"color": A, "value": .3}, {"color": R, "value": 1}]),
    stat("Live relevance (NDCG@10)", 'sum(rate(aisle_search_relevance_sum[2m])) / sum(rate(aisle_search_relevance_count[2m]))', 12, "none",
         [{"color": R, "value": None}, {"color": A, "value": .5}, {"color": G, "value": .65}],
         desc="Searches that match a labelled WANDS query are scored live. Stands in for click-through data."),
    stat("Zero-result rate", 'sum(rate(aisle_search_zero_results_total[1m])) / ' + rate + ' or vector(0)', 16, "percentunit",
         [{"color": G, "value": None}, {"color": A, "value": .02}, {"color": R, "value": .1}]),
    {"type": "stat", "title": "Running version", "datasource": P, "gridPos": {"x": 20, "y": y, "w": 4, "h": 4},
     "options": {"colorMode": "none", "textMode": "name", "reduceOptions": {"calcs": ["lastNotNull"]}},
     "targets": [{"datasource": P, "expr": 'sum by (app_version) (rate(aisle_search_duration_seconds_count[1m])) > 0',
                  "legendFormat": "{{app_version}}", "refId": "A"}]},
]
y += 4
panels += [
    ts("Search latency by mode (p50 / p95)", [
        ('histogram_quantile(0.95, sum by (le, search_mode) (rate(aisle_search_duration_seconds_bucket[1m])))', "p95 {{search_mode}}"),
        ('histogram_quantile(0.5, sum by (le, search_mode) (rate(aisle_search_duration_seconds_bucket[1m])))', "p50 {{search_mode}}"),
    ], 0, unit="s"),
    ts("Errors by version", [('sum by (app_version) (rate(aisle_search_errors_total[1m]))', "{{app_version}}"),
                             ('sum by (rpc_method, rpc_response_status_code) (rate(rpc_server_call_duration_seconds_count{rpc_response_status_code!="OK"}[1m]))', "gRPC {{rpc_method}} {{rpc_response_status_code}}")],
       12, unit="reqps"),
]
y += 8
panels += [
    ts("Live relevance by mode", [('sum by (search_mode) (rate(aisle_search_relevance_sum[2m])) / sum by (search_mode) (rate(aisle_search_relevance_count[2m]))', "{{search_mode}}")],
       0, unit="none", desc="NDCG@10 for live searches that match a labelled query"),
    ts("Database time (p95 by operation)", [('histogram_quantile(0.95, sum by (le, db_operation_name) (rate(db_client_operation_duration_seconds_bucket[1m])))', "{{db_operation_name}}")],
       12, unit="s"),
]
y += 8
panels += [
    ts("Ask (RAG) latency p95 and outcome", [
        ('histogram_quantile(0.95, sum by (le) (rate(aisle_ask_duration_seconds_bucket[5m])))', "p95"),
        ('sum by (status) (rate(aisle_ask_duration_seconds_count[5m]))', "{{status}} / s"),
    ], 0, unit="s"),
    ts("LLM tokens / min", [('sum by (kind) (rate(aisle_llm_tokens_total[5m])) * 60', "{{kind}}")], 12),
]
y += 8
panels += [
    {"type": "logs", "title": "Warnings and errors", "datasource": L, "gridPos": {"x": 0, "y": y, "w": 12, "h": 9},
     "options": {"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending"},
     "targets": [{"datasource": L, "expr": '{service_name="aisle"} | detected_level=~"warn|error"', "refId": "A"}]},
    {"type": "logs", "title": "Change events (deploys, config reloads)", "datasource": L, "gridPos": {"x": 12, "y": y, "w": 12, "h": 9},
     "options": {"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending"},
     "targets": [{"datasource": L, "expr": '{service_name="aisle"} |~ "rollout|config reloaded|maintenance"', "refId": "A"}]},
]
y += 9
panels.append({"type": "table", "title": "Slowest recent traces", "datasource": T, "gridPos": {"x": 0, "y": y, "w": 24, "h": 8},
               "targets": [{"datasource": T, "queryType": "traceql", "query": '{resource.service.name="aisle" && duration > 500ms}', "limit": 20, "refId": "A"}]})

json.dump({"uid": "aisle", "title": "Aisle: search service", "tags": ["aisle"], "timezone": "browser",
           "refresh": "10s", "time": {"from": "now-30m", "to": "now"}, "schemaVersion": 39, "panels": panels},
          open("deploy/grafana/aisle.json", "w"), indent=1)
print("wrote", len(panels), "panels")
