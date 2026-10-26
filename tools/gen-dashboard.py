#!/usr/bin/env python3
"""Generates deploy/monitoring/grafana-dashboard.json."""
import json
import pathlib

panels = []
ids = iter(range(1, 1000))


def panel(title, typ, exprs, x, y, w=12, h=8, unit=None, desc="", text_mode=None, thresholds=None,
          mappings=None, into=None):
    p = {
        "id": next(ids),
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
    if thresholds:
        # thresholds is a list of (value, color); the first value is the base.
        p["fieldConfig"]["defaults"]["thresholds"] = {"mode": "absolute", "steps": [
            {"value": v, "color": c} for v, c in thresholds]}
    if mappings:
        p["fieldConfig"]["defaults"]["mappings"] = [{"type": "value", "options": {
            str(v): {"text": t, "color": c} for v, (t, c) in mappings.items()}}]
    if typ == "timeseries":
        p["options"] = {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["mean", "max"]},
                        "tooltip": {"mode": "multi"}}
    if typ == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"]},
                        "colorMode": "background" if thresholds else "value", "graphMode": "none"}
        if text_mode:
            p["options"]["textMode"] = text_mode
    (panels if into is None else into).append(p)


def m(name, extra=""):
    sel = 'job=~"$job",server=~"$server"' + ("," + extra if extra else "")
    return "mautrix_chatto_" + name + "{" + sel + "}"


def rate(name, by, extra=""):
    return "sum by (" + by + ") (rate(" + m(name, extra) + RI + "))"


JOB = '{job=~"$job"}'
RI = "[$__rate_interval]"
OK_AT_ZERO = [(None, "green"), (1, "red")]

# Panels sum over the servers picked in $server, so "All" shows the whole
# bridge. The collapsed "Per server" row at the bottom splits by server.
y = 0
panel("Up", "stat", [("min(up" + JOB + ")", "")], 0, y, 4, 4,
      desc="Whether Prometheus can scrape every selected bridge",
      thresholds=[(None, "red"), (1, "green")], mappings={0: ("DOWN", "red"), 1: ("UP", "green")})
panel("Bridge version", "stat", [("max by (version) (mautrix_chatto_build_info" + JOB + ")", "{{version}}")],
      4, y, 4, 4, text_mode="name")
panel("Chatto server version", "stat",
      [("max by (server, version) (" + m("chatto_server_info", 'supported="true"') + " or "
        + m("chatto_server_info", 'supported="false"') + " * 0)", "{{server}} {{version}}")],
      8, y, 4, 4, text_mode="name", thresholds=[(None, "red"), (1, "green")],
      desc="Red when the server is older than the protos this bridge was built against")
panel("Connected logins", "stat", [("sum(" + m("logins", 'state="CONNECTED"') + ")", "connected")], 12, y, 4, 4)
panel("Logins without realtime", "stat",
      [("clamp_min(sum(" + m("logins", 'state="CONNECTED"') + ") - (sum(" + m("realtime_connected")
        + ") or vector(0)), 0)", "missing")], 16, y, 4, 4, thresholds=OK_AT_ZERO,
      desc="Logins that report connected but have no realtime socket")
panel("Logins needing attention", "stat",
      [("sum(" + m("logins", 'state=~"BAD_CREDENTIALS|UNKNOWN_ERROR|LOGGED_OUT"') + ")", "bad")], 20, y, 4, 4,
      thresholds=OK_AT_ZERO, desc="Logins that need the user to log in again")
y += 4
panel("Messages bridged", "timeseries",
      [(rate("messages_bridged_total", "direction, type"), "{{direction}} {{type}}")], 0, y, unit="ops")
panel("Bridge errors", "timeseries",
      [(rate("bridge_errors_total", "direction, type"), "{{direction}} {{type}}")], 12, y, unit="ops")
y += 8
panel("Error ratios", "timeseries", [
    (rate("bridge_errors_total", "direction") + " / (" + rate("messages_bridged_total", "direction") + " + "
     + rate("bridge_errors_total", "direction") + ")", "bridge {{direction}}"),
    ("sum(rate(" + m("rpc_errors_total") + RI + ")) / sum(rate(" + m("rpc_duration_seconds_count") + RI + "))",
     "rpc"),
], 0, y, unit="percentunit", desc="Share of events that failed to bridge, and of RPCs to Chatto that failed")
lag = "sum by (le) (rate(" + m("realtime_event_lag_seconds_bucket") + RI + "))"
panel("Realtime event lag", "timeseries",
      [("histogram_quantile(%s, %s)" % (q, lag), "p" + str(int(float(q) * 100))) for q in ("0.5", "0.95", "0.99")],
      12, y, unit="s", desc="Time between an event's creation on Chatto and its arrival at the bridge")
y += 8
panel("Realtime reconnects and recoveries", "timeseries", [
    ("sum by (reason) (increase(" + m("realtime_reconnects_total") + RI + "))", "reconnect: {{reason}}"),
    ("sum by (result) (increase(" + m("realtime_recoveries_total") + RI + "))", "recovery: {{result}}"),
], 0, y, unit="short", desc="Recoveries other than 'resumed' trigger a forward fill")
panel("Realtime events by type", "timeseries",
      [(rate("realtime_events_total", "type"), "{{type}}")], 12, y, unit="ops")
y += 8
panel("RPC calls by method", "timeseries",
      [(rate("rpc_duration_seconds_count", "procedure"), "{{procedure}}")], 0, y, unit="ops")
panel("RPC latency p95 by method", "timeseries",
      [("histogram_quantile(0.95, sum by (le, procedure) (rate(" + m("rpc_duration_seconds_bucket") + RI + ")))",
        "{{procedure}}")], 12, y, unit="s")
y += 8
panel("RPC errors by method and code", "timeseries",
      [(rate("rpc_errors_total", "procedure, code"), "{{procedure}} {{code}}")], 0, y, unit="ops")
panel("Token refreshes", "timeseries",
      [("sum by (result) (increase(" + m("token_refreshes_total") + RI + "))", "{{result}}")], 12, y, unit="short")
y += 8
panel("Memory", "timeseries", [("process_resident_memory_bytes" + JOB, "rss"),
                               ("go_memstats_heap_inuse_bytes" + JOB, "heap in use")], 0, y, unit="bytes")
panel("Goroutines and CPU", "timeseries", [("go_goroutines" + JOB, "goroutines"),
                                           ("rate(process_cpu_seconds_total" + JOB + RI + ")", "cpu cores")],
      12, y, unit="short")
y += 8

per_server = []
panels.append({"id": next(ids), "type": "row", "title": "Per server", "collapsed": True,
               "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "panels": per_server})
y += 1
panel("Logins by state", "timeseries",
      [("sum by (server, state) (" + m("logins") + ")", "{{server}} {{state}}")], 0, y, unit="short",
      into=per_server)
panel("Logins without realtime", "timeseries",
      [("clamp_min(sum by (server) (" + m("logins", 'state="CONNECTED"') + ") - sum by (server) ("
        + m("realtime_connected") + "), 0)", "{{server}}")], 12, y, unit="short", into=per_server)
y += 8
panel("Messages bridged", "timeseries",
      [(rate("messages_bridged_total", "server, direction"), "{{server}} {{direction}}")], 0, y, unit="ops",
      into=per_server)
panel("Bridge errors", "timeseries",
      [(rate("bridge_errors_total", "server, direction"), "{{server}} {{direction}}")], 12, y, unit="ops",
      into=per_server)
y += 8
panel("Realtime event lag p95", "timeseries",
      [("histogram_quantile(0.95, sum by (le, server) (rate(" + m("realtime_event_lag_seconds_bucket") + RI
        + ")))", "{{server}}")], 0, y, unit="s", into=per_server)
panel("RPC error ratio", "timeseries",
      [(rate("rpc_errors_total", "server") + " / " + rate("rpc_duration_seconds_count", "server"), "{{server}}")],
      12, y, unit="percentunit", into=per_server)

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
        {"name": "server", "type": "query", "label": "Chatto server",
         "datasource": {"type": "prometheus", "uid": "${datasource}"},
         "query": {"query": 'label_values(mautrix_chatto_chatto_server_info{job=~"$job"}, server)',
                   "refId": "server"},
         "definition": 'label_values(mautrix_chatto_chatto_server_info{job=~"$job"}, server)',
         "includeAll": True, "allValue": ".*", "multi": True,
         "current": {"selected": True, "text": "All", "value": "$__all"}, "refresh": 2},
    ]},
    "annotations": {"list": [
        {"name": "Restarts", "enable": True, "iconColor": "orange",
         "datasource": {"type": "prometheus", "uid": "${datasource}"},
         "expr": "changes(process_start_time_seconds" + JOB + "[2m]) > 0",
         "step": "1m", "titleFormat": "Bridge restarted", "textFormat": "{{instance}}"},
    ]},
    "panels": panels,
}
out = pathlib.Path(__file__).resolve().parent.parent / "deploy" / "monitoring" / "grafana-dashboard.json"
out.write_text(json.dumps(dashboard, indent=2) + "\n")
print("wrote", out, "with", len(panels), "panels")
