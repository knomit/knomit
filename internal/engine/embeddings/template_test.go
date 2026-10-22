package embeddings

import "testing"

func TestFillTemplate(t *testing.T) {
	cases := []struct{ tmpl, title, content, want string }{
		{"title: {title} | text: {content}", "Foo", "bar", "title: Foo | text: bar"},
		{"search_query: {content}", "", "bar", "search_query: bar"},
		{"{content}", "ignored", "bar", "bar"},
	}
	for _, c := range cases {
		if got := fillTemplate(c.tmpl, c.title, c.content); got != c.want {
			t.Errorf("fillTemplate(%q,%q,%q) = %q, want %q", c.tmpl, c.title, c.content, got, c.want)
		}
	}
}

func TestFillTemplateMissingTitleSlot(t *testing.T) {
	if got := fillTemplate("search_document: {content}", "T", "body"); got != "search_document: body" {
		t.Errorf("got %q", got)
	}
}
