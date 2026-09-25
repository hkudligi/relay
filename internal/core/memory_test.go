package core

import (
	"reflect"
	"testing"

	"github.com/harsha/relay/internal/model"
)

func TestParseMemoryUpdateEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		wantClean string
		want      model.MemoryUpdate
	}{
		{
			name:      "block at start",
			response:  `<rly-memory>{"upsert":[{"key":"start.key","value":"start value"}]}</rly-memory>Done.`,
			wantClean: "Done.",
			want: model.MemoryUpdate{Upsert: []model.MemoryEntry{
				{Key: "start.key", Value: "start value"},
			}},
		},
		{
			name:      "block at end",
			response:  `Done.<rly-memory>{"delete":["old.key"]}</rly-memory>`,
			wantClean: "Done.",
			want:      model.MemoryUpdate{Delete: []string{"old.key"}},
		},
		{
			name:      "multiple blocks uses last",
			response:  `First <rly-memory>{"upsert":[{"key":"first.key","value":"first value"}]}</rly-memory> middle <rly-memory>{"upsert":[{"key":"last.key","value":"last value"}]}</rly-memory> done`,
			wantClean: `First <rly-memory>{"upsert":[{"key":"first.key","value":"first value"}]}</rly-memory> middle  done`,
			want: model.MemoryUpdate{Upsert: []model.MemoryEntry{
				{Key: "last.key", Value: "last value"},
			}},
		},
		{
			name:      "unterminated block ignored",
			response:  `Done. <rly-memory>{"upsert":[{"key":"missing.close","value":"value"}]}`,
			wantClean: `Done. <rly-memory>{"upsert":[{"key":"missing.close","value":"value"}]}`,
		},
		{
			name:      "unknown fields ignored by preserving response",
			response:  `Done. <rly-memory>{"upsert":[{"key":"valid.key","value":"value"}],"unknown":true}</rly-memory>`,
			wantClean: `Done. <rly-memory>{"upsert":[{"key":"valid.key","value":"value"}],"unknown":true}</rly-memory>`,
		},
		{
			name:      "duplicate key across upsert and delete ignored",
			response:  `Done. <rly-memory>{"upsert":[{"key":"same.key","value":"value"}],"delete":["same.key"]}</rly-memory>`,
			wantClean: `Done. <rly-memory>{"upsert":[{"key":"same.key","value":"value"}],"delete":["same.key"]}</rly-memory>`,
		},
		{
			name:      "whitespace only payload ignored",
			response:  "Done. <rly-memory> \n\t </rly-memory>",
			wantClean: "Done. <rly-memory> \n\t </rly-memory>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClean, got := parseMemoryUpdate(tt.response)
			if gotClean != tt.wantClean {
				t.Fatalf("clean response = %q, want %q", gotClean, tt.wantClean)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("memory update = %#v, want %#v", got, tt.want)
			}
		})
	}
}
