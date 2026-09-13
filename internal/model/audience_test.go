package model

import (
	"slices"
	"testing"
)

func TestExpandAudience(t *testing.T) {
	participants := map[string][]string{
		"api":  {"lang:go", "team:platform"},
		"web":  {"lang:ts", "team:platform"},
		"docs": {"lang:md"},
	}
	tests := []struct {
		name string
		a    Audience
		want []string
	}{
		{name: "single project", a: Audience{Project: "api"}, want: []string{"api"}},
		{name: "tag expansion", a: Audience{Tag: "team:platform"}, want: []string{"api", "web"}},
		{name: "tag with one member", a: Audience{Tag: "lang:md"}, want: []string{"docs"}},
		{name: "unknown tag", a: Audience{Tag: "lang:rust"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandAudience(tt.a, participants)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ExpandAudience(%+v) = %v, want %v", tt.a, got, tt.want)
			}
		})
	}
	t.Run("all participants", func(t *testing.T) {
		got := ExpandAudience(Audience{All: true}, participants)
		slices.Sort(got)
		if !slices.Equal(got, []string{"api", "docs", "web"}) {
			t.Errorf("ExpandAudience(all) = %v", got)
		}
	})
}
