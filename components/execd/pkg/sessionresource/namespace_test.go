// Copyright 2026 Alibaba Group Holding Ltd.
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

package sessionresource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

const (
	fakePID       = 1234
	fakeStartTime = 987654
	fakeNetInode  = 4026533001
	fakeUserInode = 4026531837
)

type fakeMount struct {
	info          pathInfo
	namespaceType int
	fsType        int64
}

type fakeNamespaceOps struct {
	mu sync.Mutex

	euid     uint32
	nextFD   int
	nextIno  uint64
	nodes    map[string]pathInfo
	fds      map[int]fakeMount
	closed   map[int]bool
	mounts   map[string]fakeMount
	ops      []string
	statRead int
	stats    [][]byte

	netInfo         pathInfo
	userInfo        pathInfo
	currentUserInfo pathInfo
	ownerUID        uint32
	ownerErr        error
	failStatPath    string
	failCreateClose string
	failMount       string
	failUmount      string
}

func newFakeNamespaceOps() *fakeNamespaceOps {
	ops := &fakeNamespaceOps{
		euid:            1000,
		nextFD:          10,
		nextIno:         100,
		nodes:           make(map[string]pathInfo),
		fds:             make(map[int]fakeMount),
		closed:          make(map[int]bool),
		mounts:          make(map[string]fakeMount),
		netInfo:         pathInfo{dev: 7, inode: fakeNetInode, mode: 0o444},
		userInfo:        pathInfo{dev: 7, inode: fakeUserInode, mode: 0o444},
		currentUserInfo: pathInfo{dev: 7, inode: fakeUserInode, mode: 0o444},
		ownerUID:        1000,
		stats: [][]byte{
			fakeProcessStat(fakeStartTime),
			fakeProcessStat(fakeStartTime),
		},
	}
	return ops
}

func (o *fakeNamespaceOps) supported() bool {
	return true
}

func (o *fakeNamespaceOps) effectiveUID() uint32 {
	return o.euid
}

func (o *fakeNamespaceOps) mkdirAll(path string, mode fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.nodes[path]; ok {
		return nil
	}
	o.nodes[path] = o.newPathInfo(mode | fs.ModeDir)
	o.ops = append(o.ops, "mkdirall "+path)
	return nil
}

func (o *fakeNamespaceOps) mkdir(path string, mode fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.nodes[path]; ok {
		return fs.ErrExist
	}
	o.nodes[path] = o.newPathInfo(mode | fs.ModeDir)
	o.ops = append(o.ops, "mkdir "+path)
	return nil
}

func (o *fakeNamespaceOps) createFile(
	path string,
	mode fs.FileMode,
) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.nodes[path]; ok {
		return false, fs.ErrExist
	}
	o.nodes[path] = o.newPathInfo(mode)
	o.ops = append(o.ops, "create "+path)
	if path == o.failCreateClose {
		return true, syscall.EIO
	}
	return true, nil
}

func (o *fakeNamespaceOps) statPath(path string) (pathInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path == o.failStatPath {
		return pathInfo{}, syscall.EIO
	}
	if mount, ok := o.mounts[path]; ok {
		return mount.info, nil
	}
	info, ok := o.nodes[path]
	if !ok {
		return pathInfo{}, fs.ErrNotExist
	}
	return info, nil
}

func (o *fakeNamespaceOps) readFile(path string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != filepath.Join("/proc", strconv.Itoa(fakePID), "stat") {
		return nil, fs.ErrNotExist
	}
	if len(o.stats) == 0 {
		return nil, errors.New("no fake process stat")
	}
	index := o.statRead
	if index >= len(o.stats) {
		index = len(o.stats) - 1
	}
	o.statRead++
	return append([]byte(nil), o.stats[index]...), nil
}

func (o *fakeNamespaceOps) openNamespace(path string) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var namespace fakeMount
	switch {
	case path == filepath.Join("/proc", strconv.Itoa(fakePID), "ns", "net"):
		namespace = fakeMount{
			info:          o.netInfo,
			namespaceType: cloneNewNet,
			fsType:        nsfsSuperMagic,
		}
	case path == "/proc/self/ns/user":
		namespace = fakeMount{
			info:          o.currentUserInfo,
			namespaceType: cloneNewUser,
			fsType:        nsfsSuperMagic,
		}
	case o.mounts[path].info.inode != 0:
		namespace = o.mounts[path]
	default:
		return -1, fs.ErrNotExist
	}
	return o.allocateFD(namespace), nil
}

func (o *fakeNamespaceOps) statFD(fd int) (pathInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return pathInfo{}, syscall.EBADF
	}
	return namespace.info, nil
}

func (o *fakeNamespaceOps) fileSystemType(fd int) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return 0, syscall.EBADF
	}
	return namespace.fsType, nil
}

func (o *fakeNamespaceOps) namespaceType(fd int) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return 0, syscall.EBADF
	}
	return namespace.namespaceType, nil
}

func (o *fakeNamespaceOps) namespaceOwner(fd int) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return -1, syscall.EBADF
	}
	if namespace.namespaceType != cloneNewNet {
		return -1, syscall.EINVAL
	}
	return o.allocateFD(fakeMount{
		info:          o.userInfo,
		namespaceType: cloneNewUser,
		fsType:        nsfsSuperMagic,
	}), nil
}

func (o *fakeNamespaceOps) namespaceOwnerUID(fd int) (uint32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return 0, syscall.EBADF
	}
	if namespace.namespaceType != cloneNewUser {
		return 0, syscall.EINVAL
	}
	if o.ownerErr != nil {
		return 0, o.ownerErr
	}
	return o.ownerUID, nil
}

func (o *fakeNamespaceOps) bindMountFD(fd int, target string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if target == o.failMount {
		return syscall.EPERM
	}
	namespace, ok := o.fds[fd]
	if !ok || o.closed[fd] {
		return syscall.EBADF
	}
	if _, ok := o.nodes[target]; !ok {
		return fs.ErrNotExist
	}
	o.mounts[target] = namespace
	o.ops = append(o.ops, "mount "+target)
	return nil
}

func (o *fakeNamespaceOps) unmount(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path == o.failUmount {
		return syscall.EBUSY
	}
	if _, ok := o.mounts[path]; !ok {
		return syscall.EINVAL
	}
	delete(o.mounts, path)
	o.ops = append(o.ops, "unmount "+path)
	return nil
}

func (o *fakeNamespaceOps) remove(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, mounted := o.mounts[path]; mounted {
		return syscall.EBUSY
	}
	if _, ok := o.nodes[path]; !ok {
		return fs.ErrNotExist
	}
	for candidate := range o.nodes {
		if filepath.Dir(candidate) == path {
			return syscall.ENOTEMPTY
		}
	}
	delete(o.nodes, path)
	o.ops = append(o.ops, "remove "+path)
	return nil
}

func (o *fakeNamespaceOps) closeFD(fd int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.fds[fd]; !ok || o.closed[fd] {
		return syscall.EBADF
	}
	o.closed[fd] = true
	o.ops = append(o.ops, fmt.Sprintf("close %d", fd))
	return nil
}

func (o *fakeNamespaceOps) newPathInfo(mode fs.FileMode) pathInfo {
	o.nextIno++
	return pathInfo{
		dev:   1,
		inode: o.nextIno,
		mode:  mode,
		uid:   o.euid,
	}
}

func (o *fakeNamespaceOps) allocateFD(namespace fakeMount) int {
	fd := o.nextFD
	o.nextFD++
	o.fds[fd] = namespace
	return fd
}

func (o *fakeNamespaceOps) addDirectory(path string, uid uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	info := o.newPathInfo(0o700 | fs.ModeDir)
	info.uid = uid
	o.nodes[path] = info
}

func (o *fakeNamespaceOps) hasPath(path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, node := o.nodes[path]
	_, mounted := o.mounts[path]
	return node || mounted
}

func (o *fakeNamespaceOps) operationCount(operation string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, actual := range o.ops {
		if actual == operation {
			count++
		}
	}
	return count
}

func (o *fakeNamespaceOps) allFDsClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for fd := range o.fds {
		if !o.closed[fd] {
			return false
		}
	}
	return true
}

func fakeProcessStat(startTime uint64) []byte {
	return []byte(fmt.Sprintf(
		"%d (gate with ) chars) S 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0 0\n",
		fakePID,
		startTime,
	))
}

func fakeWorkloadIdentity() isolation.WorkloadIdentity {
	return isolation.WorkloadIdentity{
		PID:                   fakePID,
		SandboxPID:            1200,
		NetNamespaceID:        fakeNetInode,
		ProcessStartTimeTicks: fakeStartTime,
	}
}

func newFakeManager(t *testing.T, ops *fakeNamespaceOps) *NamespaceManager {
	t.Helper()
	manager, err := newNamespaceManager(
		"/run/execd/namespaces",
		ops,
		bytes.NewReader(make([]byte, namespaceIDBytes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestNamespaceManagerPinsOwningUserNamespace(t *testing.T) {
	ops := newFakeNamespaceOps()
	ops.ownerUID = 2000
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if err != nil {
		t.Fatal(err)
	}
	expectedID := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	expectedDirectory := filepath.Join(manager.root, expectedID)
	if pins.Directory() != expectedDirectory {
		t.Fatalf("directory = %q, want %q", pins.Directory(), expectedDirectory)
	}
	if pins.Identity().NetDevice != ops.netInfo.dev ||
		pins.Identity().NetInode != fakeNetInode ||
		pins.Identity().OwningUserDevice != ops.userInfo.dev ||
		pins.Identity().OwningUserInode != fakeUserInode ||
		pins.Identity().OwningUserUID != 2000 {
		t.Fatalf("identity = %+v", pins.Identity())
	}
	directoryInfo, err := ops.statPath(expectedDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.uid != ops.euid || directoryInfo.mode.Perm() != 0o700 {
		t.Fatalf("directory owner/mode = %d/%#o", directoryInfo.uid, directoryInfo.mode.Perm())
	}
	if !ops.hasPath(pins.NetPath()) || !ops.hasPath(pins.UserPath()) {
		t.Fatal("namespace pins were not published")
	}

	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if ops.hasPath(expectedDirectory) {
		t.Fatal("namespace directory survived Close")
	}
	if !ops.allFDsClosed() {
		t.Fatal("namespace descriptors survived Close")
	}
	if got := ops.operationCount("mount " + pins.NetPath()); got != 1 {
		t.Fatalf("network mounts = %d, want 1", got)
	}
	if got := ops.operationCount("mount " + pins.UserPath()); got != 1 {
		t.Fatalf("user mounts = %d, want 1", got)
	}
}

func TestNamespaceManagerUsesExecdUIDForInitialUserNamespace(t *testing.T) {
	ops := newFakeNamespaceOps()
	ops.euid = 1234
	ops.ownerErr = syscall.EPERM
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if pins.Identity().OwningUserUID != ops.euid {
		t.Fatalf(
			"owning user uid = %d, want execd uid %d",
			pins.Identity().OwningUserUID,
			ops.euid,
		)
	}
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if !ops.allFDsClosed() {
		t.Fatal("initial user namespace fallback leaked descriptors")
	}
}

func TestNamespaceManagerRejectsOwnerUIDFallbackForAnotherUserNamespace(
	t *testing.T,
) {
	ops := newFakeNamespaceOps()
	ops.ownerErr = syscall.EPERM
	ops.currentUserInfo.inode++
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("different user namespace returned pins")
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("error = %v, want %v", err, syscall.EPERM)
	}
	if !ops.allFDsClosed() {
		t.Fatal("rejected owner uid fallback leaked descriptors")
	}
}

func TestNamespaceManagerRejectsUnexpectedOwnerUIDError(t *testing.T) {
	ops := newFakeNamespaceOps()
	ops.ownerErr = syscall.EIO
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("unexpected owner uid error returned pins")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("error = %v, want %v", err, syscall.EIO)
	}
	if !ops.allFDsClosed() {
		t.Fatal("unexpected owner uid error leaked descriptors")
	}
}

func TestNamespaceManagerRejectsPIDReuse(t *testing.T) {
	ops := newFakeNamespaceOps()
	ops.stats = [][]byte{
		fakeProcessStat(fakeStartTime),
		fakeProcessStat(fakeStartTime + 1),
	}
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("PID reuse returned namespace pins")
	}
	if !errors.Is(err, ErrPIDReused) {
		t.Fatalf("error = %v, want %v", err, ErrPIDReused)
	}
	if !ops.allFDsClosed() {
		t.Fatal("PID reuse leaked namespace descriptors")
	}
}

func TestNamespaceManagerRejectsNamespaceMismatch(t *testing.T) {
	ops := newFakeNamespaceOps()
	ops.netInfo.inode++
	manager := newFakeManager(t, ops)

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("namespace mismatch returned pins")
	}
	if !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("error = %v, want %v", err, ErrNamespaceMismatch)
	}
	if !ops.allFDsClosed() {
		t.Fatal("namespace mismatch leaked descriptors")
	}
}

func TestNamespaceManagerRollsBackPartialMount(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	expectedDirectory := filepath.Join(
		manager.root,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	ops.failMount = filepath.Join(expectedDirectory, "user")

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("successful rollback returned retained pins")
	}
	if err == nil || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("error = %v, want mount failure", err)
	}
	if ops.hasPath(expectedDirectory) {
		t.Fatal("partial mount rollback retained namespace directory")
	}
	if !ops.allFDsClosed() {
		t.Fatal("partial mount rollback leaked descriptors")
	}
}

func TestNamespaceManagerReturnsCleanupOwnershipForRetry(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	expectedDirectory := filepath.Join(
		manager.root,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	ops.failMount = filepath.Join(expectedDirectory, "user")
	ops.failUmount = filepath.Join(expectedDirectory, "net")

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins == nil {
		t.Fatal("incomplete rollback lost namespace ownership")
	}
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("error = %v, want %v", err, ErrCleanupIncomplete)
	}
	if !ops.hasPath(expectedDirectory) {
		t.Fatal("incomplete rollback forgot retained directory")
	}

	ops.failUmount = ""
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if ops.hasPath(expectedDirectory) {
		t.Fatal("cleanup retry retained namespace directory")
	}
	if !ops.allFDsClosed() {
		t.Fatal("cleanup retry leaked descriptors")
	}
}

func TestNamespaceManagerRetriesDirectoryStatRollback(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	expectedDirectory := filepath.Join(
		manager.root,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	ops.failStatPath = expectedDirectory

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins == nil {
		t.Fatal("failed directory stat discarded cleanup ownership")
	}
	if !errors.Is(err, ErrCleanupIncomplete) ||
		!errors.Is(err, syscall.EIO) {
		t.Fatalf("error = %v", err)
	}
	if !ops.hasPath(expectedDirectory) {
		t.Fatal("failed directory stat lost its directory")
	}

	ops.failStatPath = ""
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if ops.hasPath(expectedDirectory) {
		t.Fatal("directory stat retry retained namespace directory")
	}
	if !ops.allFDsClosed() {
		t.Fatal("directory stat retry leaked descriptors")
	}
}

func TestNamespaceManagerRollsBackCreatedTargetAfterCloseError(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	expectedDirectory := filepath.Join(
		manager.root,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	ops.failCreateClose = filepath.Join(expectedDirectory, "net")

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("successful target rollback returned cleanup ownership")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("error = %v, want %v", err, syscall.EIO)
	}
	if ops.hasPath(expectedDirectory) {
		t.Fatal("target close failure retained namespace directory")
	}
	if !ops.allFDsClosed() {
		t.Fatal("target close failure leaked descriptors")
	}
}

func TestNamespaceManagerDoesNotDeleteOpaqueCollision(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	expectedDirectory := filepath.Join(
		manager.root,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	ops.addDirectory(expectedDirectory, ops.euid)
	sentinel := filepath.Join(expectedDirectory, "sentinel")
	if _, err := ops.createFile(sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if pins != nil {
		t.Fatal("opaque collision returned pins")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("error = %v, want %v", err, fs.ErrExist)
	}
	if !ops.hasPath(sentinel) {
		t.Fatal("opaque collision deleted pre-existing content")
	}
	if !ops.allFDsClosed() {
		t.Fatal("opaque collision leaked descriptors")
	}
}

func TestNamespacePinsCloseCanRetryBusyUnmount(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if err != nil {
		t.Fatal(err)
	}
	ops.failUmount = pins.NetPath()

	err = pins.Close()
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("Close error = %v, want %v", err, ErrCleanupIncomplete)
	}
	if !ops.hasPath(pins.Directory()) {
		t.Fatal("failed Close discarded retry ownership")
	}

	ops.failUmount = ""
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
	if ops.hasPath(pins.Directory()) {
		t.Fatal("retry retained namespace directory")
	}
}

func TestNamespacePinsCloseRefusesReplacedDirectory(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if err != nil {
		t.Fatal(err)
	}
	original, err := ops.statPath(pins.Directory())
	if err != nil {
		t.Fatal(err)
	}
	ops.mu.Lock()
	replaced := ops.nodes[pins.Directory()]
	replaced.inode++
	ops.nodes[pins.Directory()] = replaced
	ops.mu.Unlock()

	err = pins.Close()
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("Close error = %v, want %v", err, ErrCleanupIncomplete)
	}
	if got := ops.operationCount("unmount " + pins.NetPath()); got != 0 {
		t.Fatalf("replacement was unmounted %d times", got)
	}

	ops.mu.Lock()
	ops.nodes[pins.Directory()] = original
	ops.mu.Unlock()
	if err := pins.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamespacePinsCloseIsConcurrentAndIdempotent(t *testing.T) {
	ops := newFakeNamespaceOps()
	manager := newFakeManager(t, ops)
	pins, err := manager.Pin(context.Background(), fakeWorkloadIdentity())
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- pins.Close()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := ops.operationCount("unmount " + pins.NetPath()); got != 1 {
		t.Fatalf("network unmounts = %d, want 1", got)
	}
	if got := ops.operationCount("unmount " + pins.UserPath()); got != 1 {
		t.Fatalf("user unmounts = %d, want 1", got)
	}
}

func TestNewNamespaceManagerRejectsUnsafeRoots(t *testing.T) {
	for _, root := range []string{"", ".", "/"} {
		t.Run(strconv.Quote(root), func(t *testing.T) {
			manager, err := newNamespaceManager(
				root,
				newFakeNamespaceOps(),
				bytes.NewReader(make([]byte, namespaceIDBytes)),
			)
			if err == nil || manager != nil {
				t.Fatalf("manager/error = %#v/%v", manager, err)
			}
		})
	}
}
