// Package modality re-exports the Modality and TaskKind enums declared in
// internal/adapter so that the inherited model layer (model/channel.go) and
// other non-adapter packages can reference the same canonical types without
// pulling in the larger adapter package surface.
//
// Per S3 (BLUEPRINT.md §S3) we add Modality + TaskKind awareness to the
// inherited new-api Channel concept. The adapter package is the source of
// truth for these enums; this package is a thin alias layer with two goals:
//
//  1. Hide the adapter package from non-adapter callers — model/ should be
//     able to reference adapter.ModalityLLM without importing the entire
//     ProviderAdapter surface (Submit/Poll/etc.).
//  2. Provide normalization + validation helpers that downstream callers
//     (admin controller, migrations, tests) can use without re-implementing
//     them per call site.
//
// The aliases are declared with `type X = adapter.X` (Go type alias syntax),
// which means modality.Modality and adapter.Modality are interchangeable —
// no conversion is needed at call boundaries. Constants are re-declared as
// modality-package values for ergonomic dot-access (modality.LLM rather
// than adapter.ModalityLLM).
//
// This package MUST stay tiny and dependency-free beyond internal/adapter.
// If callers need richer behavior they should depend on internal/adapter
// directly.
package modality

import (
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// Modality is an alias of adapter.Modality describing the OUTPUT type a
// model produces. Values are interchangeable with adapter.Modality without
// explicit conversion.
type Modality = adapter.Modality

// TaskKind is an alias of adapter.TaskKind describing the dispatch pattern
// for invoking a model. Values are interchangeable with adapter.TaskKind.
type TaskKind = adapter.TaskKind

// Modality constants — re-declared from internal/adapter for ergonomic
// dot-access at call sites (modality.LLM vs adapter.ModalityLLM).
const (
	Image Modality = adapter.ModalityImage
	Video Modality = adapter.ModalityVideo
	Audio Modality = adapter.ModalityAudio
	Edit  Modality = adapter.ModalityEdit
	LLM   Modality = adapter.ModalityLLM
)

// TaskKind constants — re-declared from internal/adapter.
const (
	Sync      TaskKind = adapter.TaskKindSync
	Async     TaskKind = adapter.TaskKindAsync
	Streaming TaskKind = adapter.TaskKindStreaming
)

// DefaultModality is the modality assumed when an inherited channel row
// lacks an explicit value. Backwards-compat default is "llm" because the
// upstream new-api code only ever modeled LLM channels.
const DefaultModality = LLM

// DefaultTaskKind is the dispatch pattern assumed when an inherited
// channel row lacks an explicit value. Backwards-compat default is
// "streaming" because /v1/chat/completions is streaming-first.
const DefaultTaskKind = Streaming

// allModalities is the set of valid Modality values. Order matches the
// adapter package declaration so reviewers can audit at a glance.
var allModalities = map[Modality]struct{}{
	Image: {},
	Video: {},
	Audio: {},
	Edit:  {},
	LLM:   {},
}

// allTaskKinds is the set of valid TaskKind values.
var allTaskKinds = map[TaskKind]struct{}{
	Sync:      {},
	Async:     {},
	Streaming: {},
}

// ParseModality lowercases and validates s against the modality enum.
// Empty string returns DefaultModality (LLM) for backwards-compat with
// inherited channel rows that pre-date this column.
//
// Returns ok=false if s is non-empty but not a recognized modality.
func ParseModality(s string) (Modality, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(s))
	if trimmed == "" {
		return DefaultModality, true
	}
	m := Modality(trimmed)
	if _, ok := allModalities[m]; ok {
		return m, true
	}
	return "", false
}

// ParseTaskKind lowercases and validates s against the task-kind enum.
// Empty string returns DefaultTaskKind (Streaming) for backwards-compat.
//
// Returns ok=false if s is non-empty but not a recognized task kind.
func ParseTaskKind(s string) (TaskKind, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(s))
	if trimmed == "" {
		return DefaultTaskKind, true
	}
	t := TaskKind(trimmed)
	if _, ok := allTaskKinds[t]; ok {
		return t, true
	}
	return "", false
}

// IsValidModality reports whether m is one of the recognized modality
// constants. Empty string is NOT valid here — callers that want
// empty-as-default should use ParseModality instead.
func IsValidModality(m Modality) bool {
	_, ok := allModalities[m]
	return ok
}

// IsValidTaskKind reports whether t is one of the recognized task-kind
// constants. Empty string is NOT valid here — callers that want
// empty-as-default should use ParseTaskKind instead.
func IsValidTaskKind(t TaskKind) bool {
	_, ok := allTaskKinds[t]
	return ok
}

// NormalizeModality returns m if valid, DefaultModality otherwise.
// Useful for inherited rows where we want a guaranteed-valid value
// even if the DB happens to contain stale or NULL data.
func NormalizeModality(m Modality) Modality {
	if IsValidModality(m) {
		return m
	}
	return DefaultModality
}

// NormalizeTaskKind returns t if valid, DefaultTaskKind otherwise.
func NormalizeTaskKind(t TaskKind) TaskKind {
	if IsValidTaskKind(t) {
		return t
	}
	return DefaultTaskKind
}
