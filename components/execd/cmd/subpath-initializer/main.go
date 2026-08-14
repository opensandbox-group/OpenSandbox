// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/alibaba/opensandbox/execd/pkg/subpathinitializer"
)

func main() {
	planJSON := flag.String("plan-json", "", "trusted subpath initialization plan")
	fsGroup := flag.String("fs-group", "", "trusted fsGroup applied to newly created directories")
	flag.Parse()
	if *planJSON == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: opensandbox-subpath-initializer --plan-json <json> [--fs-group <gid>]")
		os.Exit(2)
	}
	group, err := parseFSGroup(*fsGroup)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	plan, err := subpathinitializer.ParsePlan(*planJSON)
	if err == nil {
		err = subpathinitializer.Apply(plan, group)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFSGroup(raw string) (*int, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 1 {
		return nil, fmt.Errorf("fs-group must be an integer between 1 and 2147483647")
	}
	fsGroup := int(parsed)
	return &fsGroup, nil
}
