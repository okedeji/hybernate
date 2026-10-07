/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var docsLink = regexp.MustCompile("https://okedeji\\.io/hybernate[^\"'`\\s)<>]*")

// Every link to the docs site that the operator or the plugin shows a person,
// in a waking page, a report, or a message, is to a page the site has. The
// site is the docs tree, built with directory URLs.
func TestDocsLinksExist(t *testing.T) {
	root := filepath.Join("..", "..")
	found := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") ||
				(filepath.Ext(path) != ".go" && filepath.Ext(path) != ".html") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, link := range docsLink.FindAllString(string(data), -1) {
				found++
				assert.True(t, docsPageExists(root, link), "%s links to %s, which the docs site doesn't have", path, link)
			}
			return nil
		})
		require.NoError(t, err)
	}
	assert.Positive(t, found, "the scan finds the links")
}

func docsPageExists(root, link string) bool {
	page, _, _ := strings.Cut(strings.TrimPrefix(link, "https://okedeji.io/hybernate"), "#")
	page = strings.Trim(page, "/")
	candidates := []string{filepath.Join(root, "docs", page, "index.md")}
	if page != "" {
		candidates = append(candidates, filepath.Join(root, "docs", page+".md"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}
