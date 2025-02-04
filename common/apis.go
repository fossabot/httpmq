// Copyright 2021-2022 The httpmq Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"fmt"

	"github.com/apex/log"
)

// RequestParam is a helper object for logging a request's parameters into its context
type RequestParam struct {
	// ID is the request ID
	ID string `json:"id"`
	// Method is the request method: DELETE, POST, PUT, GET, etc.
	Method string `json:"method" `
	// URI is the request URI
	URI string `json:"uri"`
}

// UpdateLogTags updates Apex log.Fields map with values the requests's parameters
func (i *RequestParam) UpdateLogTags(tags log.Fields) {
	tags["request_id"] = i.ID
	tags["request_method"] = i.Method
	tags["request_uri"] = fmt.Sprintf("'%s'", i.URI)
}
