package model

import "testing"

// TestChannel_GetModality_DefaultsToLLM pins the back-compat default
// declared in BLUEPRINT.md §S3: a Channel row that pre-dates the modality
// column reports the empty string for Modality, and our helper must hide
// that from callers by returning "llm".
func TestChannel_GetModality_DefaultsToLLM(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to llm", "", "llm"},
		{"explicit llm passes through", "llm", "llm"},
		{"explicit image passes through", "image", "image"},
		{"explicit video passes through", "video", "video"},
		{"explicit edit passes through", "edit", "edit"},
		{"explicit audio passes through", "audio", "audio"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := &Channel{Modality: tc.in}
			if got := ch.GetModality(); got != tc.want {
				t.Errorf("Channel{Modality:%q}.GetModality() = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestChannel_GetTaskKind_DefaultsToStreaming pins the back-compat
// default for the TaskKind column. /v1/chat/completions is streaming-
// first in the inherited code so empty must surface as "streaming".
func TestChannel_GetTaskKind_DefaultsToStreaming(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to streaming", "", "streaming"},
		{"explicit streaming passes through", "streaming", "streaming"},
		{"explicit sync passes through", "sync", "sync"},
		{"explicit async passes through", "async", "async"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := &Channel{TaskKind: tc.in}
			if got := ch.GetTaskKind(); got != tc.want {
				t.Errorf("Channel{TaskKind:%q}.GetTaskKind() = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestChannel_IsLLMChannel asserts that the LLM-routing predicate keeps
// the inherited /v1/chat/completions path live for any row that defaulted
// to Modality="" (legacy) or "llm" (explicit).
func TestChannel_IsLLMChannel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty (legacy row) is LLM", "", true},
		{"explicit llm is LLM", "llm", true},
		{"image is NOT LLM", "image", false},
		{"video is NOT LLM", "video", false},
		{"audio is NOT LLM", "audio", false},
		{"edit is NOT LLM", "edit", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := &Channel{Modality: tc.in}
			if got := ch.IsLLMChannel(); got != tc.want {
				t.Errorf("Channel{Modality:%q}.IsLLMChannel() = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
