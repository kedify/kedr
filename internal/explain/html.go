package explain

import (
	"bytes"
	_ "embed"
	"html/template"
)

//go:embed report.html
var page string

func HTML(d Document) ([]byte, error) {
	t, err := template.New("explanation").Parse(page)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	// html/template serializes the value in the script context, escaping HTML
	// delimiters. Never mark workload strings or embedded JSON as trusted JS/HTML.
	err = t.Execute(&b, d)
	return b.Bytes(), err
}
