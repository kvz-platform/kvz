// Copyright 2016 The etcd Authors
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

//go:build darwin

package fileutil

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// kvz patch: selectable sync primitive on macOS (KVZ_ETCD_SYNC):
//   full    F_FULLFSYNC: flush the drive cache on every WAL commit (upstream default)
//   barrier F_BARRIERFSYNC: write ordering barrier; crash-consistent, the most
//           recent commits may be lost on sudden power loss (Apple's guidance
//           for databases; what a Linux guest's fsync effectively gets on VZ)
//   none    no sync
var (
	syncOnce sync.Once
	syncMode = "full"
)

func Fsync(f *os.File) error {
	syncOnce.Do(func() {
		if m := os.Getenv("KVZ_ETCD_SYNC"); m != "" {
			syncMode = m
		}
	})
	switch syncMode {
	case "none":
		return nil
	case "barrier":
		if _, err := unix.FcntlInt(f.Fd(), unix.F_BARRIERFSYNC, 0); err == nil {
			return nil
		}
	}
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}

func Fdatasync(f *os.File) error {
	return Fsync(f)
}
