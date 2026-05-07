package controller

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
)

// TestValidateChannel_ModalityNormalization confirms that the admin
// validateChannel hook accepts empty modality/task_kind input (defaulting
// to the back-compat values) and rejects any non-empty value that is not
// in the enum. This is the BLUEPRINT.md §S3 nullability contract:
// "New fields are NULLABLE on input — defaulting to 'llm'/'streaming'
// when omitted."
//
// We use isAdd=false to bypass the key/model length checks that aren't
// relevant to modality validation; the Channel struct is otherwise empty.
func TestValidateChannel_ModalityNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		modalityIn       string
		taskKindIn       string
		wantErrSubstring string // empty = expect success
		wantModality     string
		wantTaskKind     string
	}{
		{
			name:         "empty modality and task_kind default to llm/streaming",
			modalityIn:   "",
			taskKindIn:   "",
			wantModality: "llm",
			wantTaskKind: "streaming",
		},
		{
			name:         "explicit image and async pass through",
			modalityIn:   "image",
			taskKindIn:   "async",
			wantModality: "image",
			wantTaskKind: "async",
		},
		{
			name:         "case-insensitive accepted and lowercased",
			modalityIn:   "VIDEO",
			taskKindIn:   "SYNC",
			wantModality: "video",
			wantTaskKind: "sync",
		},
		{
			name:             "unknown modality rejected",
			modalityIn:       "movie",
			taskKindIn:       "streaming",
			wantErrSubstring: "invalid modality",
		},
		{
			name:             "unknown task_kind rejected",
			modalityIn:       "image",
			taskKindIn:       "queued",
			wantErrSubstring: "invalid task_kind",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ch := &model.Channel{
				Modality: tc.modalityIn,
				TaskKind: tc.taskKindIn,
			}

			err := validateChannel(ch, false /* isAdd */)

			if tc.wantErrSubstring != "" {
				if err == nil {
					t.Fatalf("validateChannel should have errored for input %+v, got nil", tc)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Fatalf("validateChannel error %q does not contain %q", err.Error(), tc.wantErrSubstring)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateChannel returned unexpected error: %v", err)
			}
			if ch.Modality != tc.wantModality {
				t.Errorf("after validate: Modality = %q, want %q", ch.Modality, tc.wantModality)
			}
			if ch.TaskKind != tc.wantTaskKind {
				t.Errorf("after validate: TaskKind = %q, want %q", ch.TaskKind, tc.wantTaskKind)
			}
		})
	}
}
