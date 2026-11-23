package cmd

import (
	"errors"
	"strconv"
	"strings"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/NemoNobody/velruna/internal/secretguard"
)

// guardField is one user-supplied text that the secret guard inspects on a
// path where the text cannot be rewritten (a fetched URL, a document title):
// any finding refuses the write. The fact write paths use guardSanitize
// instead, which may replace a flagged span rather than lose the whole write.
type guardField struct {
	Name string
	Text string
	// HighOnly narrows the scan to high-confidence shapes: fields that carry
	// identity (a document URL) cannot be redacted and legitimately contain
	// medium-shape content (git SHAs in paths), so only the deterministic
	// credential shapes may refuse them.
	HighOnly bool
}

// guardKind picks the policy for one field of a write.
type guardKind int

const (
	// guardText is a field the guard may rewrite: losing one span (a hash that
	// tripped a heuristic) is a smaller loss than losing the whole fact.
	guardText guardKind = iota
	// guardIdentity is a field callers match on (key, project, tags, source).
	// Replacing a span would silently change what the fact IS, so a finding
	// refuses the write instead.
	guardIdentity
)

// guardTarget is one field of a write under the secret guard. dst points at
// the value that will be stored, so a redaction lands in the caller's variable
// with no read-back step.
type guardTarget struct {
	name string
	dst  *string
	kind guardKind
}

// textField marks a rewritable field (value, notes, summary, reason).
func textField(name string, dst *string) *guardTarget {
	return &guardTarget{name: name, dst: dst, kind: guardText}
}

// identityField marks a field that identifies the fact (key, project,
// source, tags): it is matched on, so it is never rewritten.
func identityField(name string, dst *string) *guardTarget {
	return &guardTarget{name: name, dst: dst, kind: guardIdentity}
}

// guardSanitize applies the write-time secret policy and rewrites the text
// fields in place. The store is plaintext: a secret written once is exposed in
// every later read, so the write is the last place it can be stopped.
//
// reject: a high-confidence shape (a real credential) refuses the write; a
// medium-confidence shape (a heuristic: long hex, base64, a weak assignment)
// is replaced with a marker and the write proceeds; a finding in an identity
// field refuses. There is no confirmation flag in this mode.
// ask: the same, except an explicit confirmation (confirm_secret /
// --allow-secret, only after the user approved the exact finding) stores the
// text as written — the operator judged the finding a false positive.
// allow: the operator opted out; nothing is scanned.
//
// Redaction is never silent: the returned audit rows name the field, the
// pattern and the chars (never the text) and travel with the stored fact.
//
// The error lists masked findings only — echoing the matched text back would
// leak it a second time through the error path.
func guardSanitize(level string, confirmed bool, targets ...*guardTarget) ([]model.Redaction, error) {
	if level == config.GuardAllow {
		return nil, nil
	}
	if level == config.GuardAsk && confirmed {
		// The human saw the masked finding and approved this write: store it as
		// written. Confirmation is per call (the batch rule: one approved false
		// positive never whitewashes the next item).
		return nil, nil
	}

	var refused []secretguard.Finding
	var redactions []model.Redaction
	for _, t := range targets {
		if t == nil || t.dst == nil || *t.dst == "" {
			continue
		}
		findings := secretguard.Scan(t.name, *t.dst)
		if len(findings) == 0 {
			continue
		}
		if t.kind == guardIdentity {
			refused = append(refused, findings...)
			continue
		}
		var high []secretguard.Finding
		for _, f := range findings {
			if f.Confidence == secretguard.ConfHigh {
				high = append(high, f)
			}
		}
		if len(high) > 0 {
			refused = append(refused, high...)
			continue // a credential in this field: leave the text whole for the message
		}
		redacted, applied := secretguard.RedactAll(t.name, *t.dst)
		if len(applied) == 0 {
			continue
		}
		if !secretguard.HasContent(redacted) {
			r := applied[0]
			return nil, errors.New("secret-guard: field " + t.name + " is nothing but a " + r.Pattern +
				" span (" + strconv.Itoa(r.Length) + " chars); redacting it would leave no fact to store.\n" +
				"Rewrite the value in words (or store it under secret_guard=allow if it is genuinely a hash).")
		}
		*t.dst = redacted
		for _, f := range applied {
			redactions = append(redactions, model.Redaction{Field: f.Field, Pattern: f.Pattern, Chars: f.Length})
		}
	}
	if len(refused) > 0 {
		return nil, guardRefusal(level, refused)
	}
	return redactions, nil
}

// guardRefusal renders the refusal message: masked findings plus the way out
// that actually exists in this mode.
func guardRefusal(level string, findings []secretguard.Finding) error {
	var b strings.Builder
	b.WriteString("secret-guard: probable secret(s) detected; the write was NOT stored.\n")
	for _, fd := range findings {
		b.WriteString("  - field ")
		b.WriteString(fd.Field)
		b.WriteString(": pattern ")
		b.WriteString(fd.Pattern)
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(fd.Length))
		b.WriteString(" chars, preview ")
		b.WriteString(fd.Masked)
		b.WriteString("\n")
	}
	b.WriteString("The store is plaintext: every later read exposes this text. ")
	if level == config.GuardAsk {
		b.WriteString("If the finding is a false positive, show it to the user and re-run with ")
		b.WriteString("--allow-secret (CLI) or confirm_secret=true (MCP) only after their explicit approval.")
	} else {
		b.WriteString("Remove the flagged text and retry; this mode (secret_guard=reject) has no confirmation flag. ")
		b.WriteString("A match in a key, project, source, tag or a superseded_by link cannot be redacted (those fields are matched on): reword it.")
	}
	return errors.New(b.String())
}

// redactionLines renders the audit rows for a human reading CLI output. It
// names the field, the pattern and the chars: the removed text must not
// travel back
// into the terminal (or the shell history).
func redactionLines(rs []model.Redaction) string {
	if len(rs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("secret-guard: the stored text was shortened — flagged spans replaced before the write:\n")
	for _, r := range rs {
		b.WriteString("  - field ")
		b.WriteString(r.Field)
		b.WriteString(": ")
		b.WriteString(r.Pattern)
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(r.Chars))
		b.WriteString(" chars\n")
	}
	return b.String()
}

// guardCheck refuses a write when any of the given fields carries a finding.
// It is the path for text that cannot be rewritten (a fetched URL, a document
// title): unlike guardSanitize it never redacts, so a finding always refuses.
//
// reject: credential-like text is never stored, no confirmation exists.
// ask: the write proceeds only with an explicit confirmation (the operator
// saw the masked finding and approved it).
// allow: the operator opted out; nothing is scanned.
func guardCheck(level string, allowed bool, fields ...guardField) error {
	if level == config.GuardAllow {
		return nil
	}
	var findings []secretguard.Finding
	for _, f := range fields {
		for _, fd := range secretguard.Scan(f.Name, f.Text) {
			if f.HighOnly && fd.Confidence != secretguard.ConfHigh {
				continue
			}
			findings = append(findings, fd)
		}
	}
	if len(findings) == 0 {
		return nil
	}
	if level == config.GuardAsk && allowed {
		return nil
	}
	return guardRefusal(level, findings)
}
