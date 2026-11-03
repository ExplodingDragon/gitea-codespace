// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"golang.org/x/text/language"
)

var (
	supportedLocaleNames = []string{"en-US", "zh-CN", "ja-JP", "ko-KR"}
	supportedLocaleTags  = []language.Tag{language.AmericanEnglish, language.SimplifiedChinese, language.Japanese, language.Korean}
	localeMatcher        = language.NewMatcher(supportedLocaleTags)
	messageCatalogs      = loadMessageCatalogs()
)

func loadMessageCatalogs() map[string]map[string]string {
	result := make(map[string]map[string]string, len(supportedLocaleNames))
	for _, name := range supportedLocaleNames {
		data, err := fs.ReadFile(assets, "locales/"+name+".json")
		if err != nil {
			panic(fmt.Sprintf("read %s locale: %v", name, err))
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			panic(fmt.Sprintf("parse %s locale: %v", name, err))
		}
		catalog := make(map[string]string)
		var flatten func(string, map[string]any)
		flatten = func(prefix string, object map[string]any) {
			for key, value := range object {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				switch value := value.(type) {
				case string:
					catalog[path] = value
				case map[string]any:
					flatten(path, value)
				default:
					panic(fmt.Sprintf("locale %s message %s must be a string or object", name, path))
				}
			}
		}
		flatten("", document)
		result[name] = catalog
	}
	base := result[supportedLocaleNames[0]]
	for _, name := range supportedLocaleNames[1:] {
		if len(result[name]) != len(base) {
			panic(fmt.Sprintf("locale %s has %d messages; English has %d", name, len(result[name]), len(base)))
		}
	}
	for key := range base {
		for _, name := range supportedLocaleNames[1:] {
			if _, ok := result[name][key]; !ok {
				panic(fmt.Sprintf("locale %s is missing message %s", name, key))
			}
		}
	}
	return result
}

// Localize returns an administration message for an HTTP Accept-Language value.
// English is the fallback for clients that do not request a supported language.
func Localize(acceptLanguage, key string, arguments map[string]string) string {
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		tags = []language.Tag{language.AmericanEnglish}
	}
	_, index, _ := localeMatcher.Match(tags...)
	message, ok := messageCatalogs[supportedLocaleNames[index]][key]
	if !ok {
		message = messageCatalogs[supportedLocaleNames[0]][key]
	}
	if message == "" {
		return key
	}
	for name, value := range arguments {
		message = strings.ReplaceAll(message, "{"+name+"}", value)
	}
	return message
}
