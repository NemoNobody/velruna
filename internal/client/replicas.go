package client

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Replica reconciliation. The replica count is otherwise frozen
// at creation time: an install that starts on one node and later grows into a
// cluster — the documented compose→kube growth path — would keep replicas=0
// forever, in the existing indices and in the journal template that stamps
// every new history index. Reconciling once per process makes "replicas
// follow the topology" a steady-state property instead of a cold-start one.

// desiredReplicas returns the replica count the current topology wants: the
// explicit config value when set, otherwise the auto probe. ok=false only in
// auto mode when the health probe failed — the caller must then NOT "fix"
// replicas toward an unknown topology (a transient probe error must never
// downgrade a real cluster to 0).
func (c *Client) desiredReplicas(ctx context.Context) (int, bool) {
	if c.cfg.IndexReplicas >= 0 {
		return c.cfg.IndexReplicas, true
	}
	return c.autoReplicas(ctx)
}

// autoReplicas caches the health probe (the topology does not change within
// one process run) and reports whether the probe answered at all.
func (c *Client) autoReplicas(ctx context.Context) (int, bool) {
	c.replicasOnce.Do(func() {
		c.replicasVal = 0
		if h, err := c.api.Cluster.Health(ctx, nil); err == nil && h.NumberOfDataNodes > 0 {
			// A probe that answers with no data nodes is not a trustworthy
			// topology: "fixing" toward 0 off a zero-node reply would strip
			// replicas from a real cluster mid-blip, the same harm as a
			// failed probe.
			c.replicasProbeOK = true
			if h.NumberOfDataNodes > 1 {
				c.replicasVal = 1
			}
		}
	})
	return c.replicasVal, c.replicasProbeOK
}

// reconciledStores claims one reconcile pass per store per process: the MCP
// server builds a fresh client per tool call, so a per-client once is not a
// process-level dedup — every call would pay the probe + settings GETs
// again. The key is URL|IndexName; the claiming client performs the pass,
// every other client for the same store skips it. The topology does not
// change within one process run, so one pass is enough.
var reconciledStores sync.Map

func storeAlreadyReconciled(cfg *config.Config) bool {
	_, loaded := reconciledStores.LoadOrStore(storeKey(cfg)+"|"+cfg.IndexName, struct{}{})
	return loaded
}

// reconcileReplicas aligns existing velruna indices with the desired count.
// Called once per process from EnsureIndex; it is best-effort by design — a
// failed settings write (read-only store, blip) never fails the operation the
// caller actually asked for.
func (c *Client) reconcileReplicas(ctx context.Context) {
	if storeAlreadyReconciled(c.cfg) {
		return
	}
	desired, ok := c.desiredReplicas(ctx)
	if !ok {
		// Unknown topology: do not consume the process-wide claim, the next
		// client may get a trustworthy probe.
		reconciledStores.Delete(c.storeKey() + "|" + c.cfg.IndexName)
		return
	}
	c.replicasFixOnce.Do(func() {
		c.fixIndexReplicas(ctx, c.cfg.IndexName, desired)
		c.fixIndexReplicas(ctx, c.cfg.DocsIndexName, desired)
		c.fixHistoryReplicas(ctx, desired)
	})
}

// fixIndexReplicas reads the live replica count and writes it back only on
// drift. A missing index (the docs index is created lazily) or an unreadable
// setting is skipped: there is nothing to align or nothing trustworthy to
// align against.
func (c *Client) fixIndexReplicas(ctx context.Context, index string, desired int) {
	if index == "" {
		return
	}
	flat := true
	get, err := c.api.Indices.Settings.Get(ctx, &opensearchapi.SettingsGetReq{
		Indices: []string{index},
		Params:  opensearchapi.SettingsGetParams{FlatSettings: &flat},
	})
	if err != nil {
		return
	}
	idx, ok := get.GetIndices()[index]
	if !ok {
		return
	}
	if repDrifted(idx.Settings, desired) {
		c.putReplicas(ctx, index, desired)
	}
}

// fixHistoryReplicas closes the same loop for the period indices the journal
// template already stamped: a cluster that grew after they were created must
// replicate their history too, not only future periods. One wildcard GET, a
// corrective PUT only for the indices that actually drifted.
func (c *Client) fixHistoryReplicas(ctx context.Context, desired int) {
	flat := true
	get, err := c.api.Indices.Settings.Get(ctx, &opensearchapi.SettingsGetReq{
		Indices: []string{journalWildcard(c.cfg.IndexName)},
		Params:  opensearchapi.SettingsGetParams{FlatSettings: &flat},
	})
	if err != nil {
		return
	}
	for name, idx := range get.GetIndices() {
		if repDrifted(idx.Settings, desired) {
			c.putReplicas(ctx, name, desired)
		}
	}
}

// repDrifted reports whether an index settings blob disagrees with the desired
// replica count. Unparseable or absent counts are "no opinion": never write
// toward a number we could not read back.
func repDrifted(settings json.RawMessage, desired int) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(settings, &m); err != nil {
		return false
	}
	cur, ok := settingInt(m["index.number_of_replicas"])
	return ok && cur != desired
}

func (c *Client) putReplicas(ctx context.Context, index string, desired int) {
	body, _ := json.Marshal(map[string]interface{}{"index.number_of_replicas": desired})
	_, _ = c.api.Indices.Settings.Put(ctx, opensearchapi.SettingsPutReq{
		Indices: []string{index},
		Body:    bytes.NewReader(body),
	})
}

// templateStampedReplicas extracts the replica count baked into the journal
// index template (-1 = absent/unparseable, treated as "no opinion"). The
// template GET returns NESTED settings regardless of flat_settings (only
// index _settings answers flat), so the nested form is tried first and the
// flat key is a fallback.
func templateStampedReplicas(resp *opensearchapi.IndexTemplateGetResp) int {
	if resp == nil || len(resp.IndexTemplates) == 0 {
		return -1
	}
	raw := resp.IndexTemplates[0].IndexTemplate.Template.Settings
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err == nil {
		if n, ok := settingInt(flat["index.number_of_replicas"]); ok {
			return n
		}
	}
	var nested struct {
		Index struct {
			NumberOfReplicas json.RawMessage `json:"number_of_replicas"`
		} `json:"index"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil {
		if n, ok := settingInt(nested.Index.NumberOfReplicas); ok {
			return n
		}
	}
	return -1
}

// settingInt reads a flat setting that may arrive as a JSON string ("1") or
// a bare number (1).
func settingInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}
