// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBasic(t *testing.T) {
	tdir := t.TempDir()

	// Do a build of . into <tmpdir>/out.exe
	exe := filepath.Join(tdir, "out.exe")
	gotoolpath := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(gotoolpath, "build", "-o", exe, ".")
	//t.Logf("cmd: %+v\n", cmd)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Logf("build: %s\n", b)
		t.Fatalf("build error: %v", err)
	}

	// Run self on self.
	dotp := filepath.Join(tdir, "out.dot")
	cmd = exec.Command(exe, "-polyline", "-dotout="+dotp, "-tgt=github.com/thanm/pcritical")
	//t.Logf("cmd: %+v\n", cmd)
	var output string
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Logf("run: %s\n", b)
		t.Fatalf("run error: %v", err)
	} else {
		output = string(b)
	}

	// Check for critical path.
	lines := strings.Split(output, "\n")
	critpath := []string{}
	cap := false
	for _, line := range lines {
		if line == "Critical path:" {
			cap = true
			continue
		}
		if cap {
			if line == "" {
				break
			}
			fs := strings.Fields(line)
			critpath = append(critpath, fs[0])
		}
	}

	//t.Logf("cp: %+v\n", critpath)
	want0 := "github.com/thanm/pcritical"
	if !strings.Contains(critpath[0], want0) {
		t.Errorf("critpath[0] got %s want %s", critpath[0], want0)
	}
	wantlast := "runtime/internal/atomic"
	cpl := critpath[len(critpath)-1]
	if !strings.Contains(cpl, wantlast) {
		t.Errorf("critpath[last] got %s want %s", cpl, wantlast)
	}
}
