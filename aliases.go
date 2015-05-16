/*
 * Minio Client (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/minio/minio/pkg/iodine"
)

// validAliasURL: use net/url.Parse to validate
var validAliasName = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9-]+$")

// Check if it is an aliased URL
func isValidAliasName(aliasName string) bool {
	// help is reserved argument
	switch aliasName {
	case "help":
		fallthrough
	case "private":
		fallthrough
	case "read-only":
		fallthrough
	case "public-read":
		fallthrough
	case "public-read-write":
		return false
	}
	return validAliasName.MatchString(aliasName)
}

// aliasExpand expands aliased (name:/path) to full URL, used by url-parser
func aliasExpand(aliasedURL string, aliases map[string]string) (newURL string, err error) {
	u, err := url.Parse(aliasedURL)
	if err != nil {
		return aliasedURL, iodine.New(errInvalidURL{url: aliasedURL}, nil)
	}
	// proper URL
	if u.Host != "" {
		return aliasedURL, nil
	}
	for aliasName, expandedURL := range aliases {
		if strings.HasPrefix(aliasedURL, aliasName+":") {
			// Match found. Expand it.
			splits := strings.Split(aliasedURL, ":")
			return expandedURL + "/" + splits[1], nil
		}
	}
	return aliasedURL, nil
}