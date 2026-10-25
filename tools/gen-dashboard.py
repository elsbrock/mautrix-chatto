#!/usr/bin/env python3
"""Generates deploy/monitoring/grafana-dashboard.json."""
import json
import pathlib

panels = []


def panel(title, typ, exprs, x, y, w=12, h=8, unit=None, desc="", text_mode=None):
    p = {
        "id": len(panels) + 1,
        "title": title,
        "type": typ,
        "description": desc,
        "datasource": {"type": "prometheus", "uid": "${datasource}"},
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [
            {"refId": chr(65 + i), "expr": e, "legendFormat": legend,
             "datasource": {"type": "prometheus", "uid": "${datasource}"}}
            for i, (e, legend) in enumerate(exprs)
        ],
        "fieldConfig": {"defaults": {}, "overrides": []},
        "options": {},
    }
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if typ == "timeseries":
        p["options"] = {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["mean", "max"]},
                        "tooltip": {"mode": "multi"}}
    if typ == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "none"}
        if text_mode:
            p["options"]["textMode"] = text_mode
    panels.append(p)


def m(name, extra=""):
    sel = 'job=~"$job"' + ("," + extra if extra else "")
    return "mautrix_chatto_" + name + "{" + sel + "}"


JOB = '{job=~"$job"}'
RI = "[$__rate_interval]"

y = 0
panel("Connected logins", "stat", [("sum(" + m("logins", 'state="CONNECTED"') + ")", "connected")], 0, y, 6, 4)
panel("Logins needing attention", "stat",
      [("sum(" + m("logins", 'state=~"BAD_CREDENTIALS|UNKNOWN_ERROR|LOGGED_OUT"') + ")", "bad")], 6, y, 6, 4,
      desc="Logins that need the user to log in again")
panel("Realtime sockets up", "stat", [("sum(" + m("realtime_connected") + ")", "up")], 12, y, 6, 4)
panel("Chatto server version", "stat", [("max by (version) (" + m("chatto_server_info") + ")", "{{version}}")],
      18, y, 6, 4, text_mode="name")
y += 4
panel("Messages bridged", "timeseries",
      [("sum by (direction, type) (rate(" + m("messages_bridged_total") + RI + "))", "{{direction}} {{type}}")],
      0, y, unit="ops")
panel("Bridge errors", "timeseries",
      [("sum by (direction, type) (rate(" + m("bridge_errors_total") + RI + "))", "{{direction}} {{type}}")],
      12, y, unit="ops")
y += 8
lag = "sum by (le) (rate(" + m("realtime_event_lag_seconds_bucket") + RI + "))"
panel("Realtime event lag", "timeseries",
      [("histogram_quantile(%s, %s)" % (q, lag), "p" + str(int(float(q) * 100))) for q in ("0.5", "0.95", "0.99")],
      0, y, unit="s", desc="Time between an event's creation on Chatto and its arrival at the bridge")
panel("Realtime reconnects and recoveries", "timeseries", [
    ("sum by (reason) (increase(" + m("realtime_reconnects_total") + RI + "))", "reconnect: {{reason}}"),
    ("sum by (result) (increase(" + m("realtime_recoveries_total") + RI + "))", "recovery: {{result}}"),
], 12, y, unit="short", desc="Recoveries other than 'resumed' trigger a forward fill")
y += 8
panel("Realtime events by type", "timeseries",
      [("sum by (type) (rate(" + m("realtime_events_total") + RI + "))", "{{type}}")], 0, y, unit="ops")
panel("RPC latency p95 by method", "timeseries",
      [("histogram_quantile(0.95, sum by (le, procedure) (rate(" + m("rpc_duration_seconds_bucket") + RI + ")))",
        "{{procedure}}")], 12, y, unit="s")
y += 8
panel("RPC errors by method and code", "timeseries",
      [("sum by (procedure, code) (rate(" + m("rpc_errors_total") + RI + "))", "{{procedure}} {{code}}")],
      0, y, unit="ops")
panel("Token refreshes", "timeseries",
      [("sum by (result) (increase(" + m("token_refreshes_total") + RI + "))", "{{result}}")], 12, y, unit="short")
y += 8
panel("Memory", "timeseries", [("process_resident_memory_bytes" + JOB, "rss"),
                               ("go_memstats_heap_inuse_bytes" + JOB, "heap in use")], 0, y, unit="bytes")
panel("Goroutines and CPU", "timeseries", [("go_goroutines" + JOB, "goroutines"),
                                           ("rate(process_cpu_seconds_total" + JOB + RI + ")", "cpu cores")],
      12, y, unit="short")

dashboard = {
    "title": "mautrix-chatto",
    "uid": "mautrix-chatto",
    "schemaVersion": 39,
    "version": 1,
    "editable": True,
    "tags": ["matrix", "chatto", "bridge"],
    "time": {"from": "now-6h", "to": "now"},
    "refresh": "30s",
    "templating": {"list": [
        {"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Data source"},
        {"name": "job", "type": "query", "label": "Job",
         "datasource": {"type": "prometheus", "uid": "${datasource}"},
         "query": {"query": "label_values(mautrix_chatto_build_info, job)", "refId": "job"},
         "definition": "label_values(mautrix_chatto_build_info, job)",
         "includeAll": True, "multi": True,
         "current": {"selected": True, "text": "All", "value": "$__all"}, "refresh": 2},
    ]},
    "panels": panels,
}
out = pathlib.Path(__file__).resolve().parent.parent / "deploy" / "monitoring" / "grafana-dashboard.json"
out.write_text(json.dumps(dashboard, indent=2) + "\n")
print("wrote", out, "with", len(panels), "panels")
