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

// Package sessionresource owns host-side resources that must remain stable for
// the lifetime of an isolated session.
package sessionresource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

const (
	// DefaultNamespaceRoot is private to execd and is never bind-mounted into
	// an isolated workload.
	DefaultNamespaceRoot = "/run/execd/namespaces"

	namespaceIDBytes = 32
	nsfsSuperMagic   = int64(0x6e736673)

	cloneNewUser = 0x10000000
	cloneNewNet  = 0x40000000
)

var (
	// ErrUnsupported means the host cannot provide the Linux namespace
	// primitives needed to create stable namespace pins.
	ErrUnsupported = errors.New("session namespace pinning is unsupported")

	// ErrInvalidIdentity means the authenticated workload identity is missing
	// a field required to defend against PID reuse.
	ErrInvalidIdentity = errors.New("invalid workload identity")

	// ErrPIDReused means procfs no longer describes the process authenticated
	// by the workload startup protocol.
	ErrPIDReused = errors.New("workload pid was reused")

	// ErrNamespaceMismatch means the namespace opened through procfs does not
	// match the inode authenticated by the workload startup protocol.
	ErrNamespaceMismatch = errors.New("workload network namespace mismatch")

	// ErrCleanupIncomplete means at least one namespace bind mount, pin target,
	// or descriptor is still owned by NamespacePins and Close must be retried.
	ErrCleanupIncomplete = errors.New("session namespace cleanup is incomplete")
)

// NamespaceIdentity records the authenticated process and namespace identity
// behind one pair of stable pins.
type NamespaceIdentity struct {
	PID                   int
	ProcessStartTimeTicks uint64
	NetDevice             uint64
	NetInode              uint64
	OwningUserDevice      uint64
	OwningUserInode       uint64
	OwningUserUID         uint32
}

type pathInfo struct {
	dev   uint64
	inode uint64
	mode  fs.FileMode
	uid   uint32
}

func sameObject(left, right pathInfo) bool {
	return left.dev != 0 &&
		left.inode != 0 &&
		left.dev == right.dev &&
		left.inode == right.inode
}

type namespaceOps interface {
	supported() bool
	effectiveUID() uint32
	mkdirAll(path string, mode fs.FileMode) error
	mkdir(path string, mode fs.FileMode) error
	createFile(path string, mode fs.FileMode) (created bool, err error)
	statPath(path string) (pathInfo, error)
	readFile(path string) ([]byte, error)
	openNamespace(path string) (int, error)
	statFD(fd int) (pathInfo, error)
	fileSystemType(fd int) (int64, error)
	namespaceType(fd int) (int, error)
	namespaceOwner(fd int) (int, error)
	namespaceOwnerUID(fd int) (uint32, error)
	bindMountFD(fd int, target string) error
	unmount(path string) error
	remove(path string) error
	closeFD(fd int) error
}

// NamespaceManager creates per-session namespace pins below a trusted root.
type NamespaceManager struct {
	root   string
	ops    namespaceOps
	random io.Reader
}

// NewNamespaceManager returns a namespace manager. The trusted root is created
// lazily on the first Pin call so ordinary sessions pay no filesystem cost.
func NewNamespaceManager(root string) (*NamespaceManager, error) {
	return newNamespaceManager(root, platformNamespaceOps{}, rand.Reader)
}

func newNamespaceManager(
	root string,
	ops namespaceOps,
	random io.Reader,
) (*NamespaceManager, error) {
	cleanRoot := filepath.Clean(root)
	if root == "" || !filepath.IsAbs(cleanRoot) || cleanRoot == string(filepath.Separator) {
		return nil, fmt.Errorf("invalid namespace root %q", root)
	}
	if ops == nil {
		return nil, errors.New("nil namespace platform operations")
	}
	if random == nil {
		return nil, errors.New("nil namespace identifier source")
	}
	return &NamespaceManager{
		root:   cleanRoot,
		ops:    ops,
		random: random,
	}, nil
}

// NamespacePins are stable server-owned bind mounts for one session.
//
// Close is concurrent-safe, idempotent, and retryable. When Pin returns both a
// non-nil NamespacePins and an error, the caller must retain the pins and call
// Close again after stopping the workload.
type NamespacePins struct {
	directory string
	netPath   string
	userPath  string
	identity  NamespaceIdentity

	ops namespaceOps

	mu sync.Mutex

	netFD  int
	userFD int

	directoryInfo pathInfo

	directoryCreated  bool
	netTargetCreated  bool
	userTargetCreated bool
	netMounted        bool
	userMounted       bool
	closed            bool
}

// Directory returns the server-generated namespace pin directory.
func (p *NamespacePins) Directory() string {
	if p == nil {
		return ""
	}
	return p.directory
}

// NetPath returns the stable network namespace pin path.
func (p *NamespacePins) NetPath() string {
	if p == nil {
		return ""
	}
	return p.netPath
}

// UserPath returns the stable owning user namespace pin path.
func (p *NamespacePins) UserPath() string {
	if p == nil {
		return ""
	}
	return p.userPath
}

// Identity returns the immutable identity captured while creating the pins.
func (p *NamespacePins) Identity() NamespaceIdentity {
	if p == nil {
		return NamespaceIdentity{}
	}
	return p.identity
}

// Pin opens the authenticated workload NetNS, obtains its owning UserNS via
// NS_GET_USERNS, validates both descriptors, and bind-pins them under an
// unpredictable server-owned path.
//
// A non-nil pins value returned with an error owns cleanup that could not be
// completed while rolling back. The caller must retain it and retry Close.
func (m *NamespaceManager) Pin(
	ctx context.Context,
	workload isolation.WorkloadIdentity,
) (_ *NamespacePins, err error) {
	if !m.ops.supported() {
		return nil, ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	identity, netFD, userFD, err := acquireNamespaceIdentity(m.ops, workload)
	if err != nil {
		return nil, err
	}
	pins := &NamespacePins{
		identity: identity,
		ops:      m.ops,
		netFD:    netFD,
		userFD:   userFD,
	}

	fail := func(pinErr error) (*NamespacePins, error) {
		cleanupErr := pins.Close()
		if cleanupErr != nil {
			return pins, errors.Join(
				pinErr,
				ErrCleanupIncomplete,
				fmt.Errorf("rollback namespace pins: %w", cleanupErr),
			)
		}
		return nil, pinErr
	}

	if err := m.ensureRoot(); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	opaqueID, err := randomNamespaceID(m.random)
	if err != nil {
		return fail(err)
	}
	pins.directory = filepath.Join(m.root, opaqueID)
	pins.netPath = filepath.Join(pins.directory, "net")
	pins.userPath = filepath.Join(pins.directory, "user")

	if err := m.ops.mkdir(pins.directory, 0o700); err != nil {
		return fail(fmt.Errorf("create opaque namespace directory: %w", err))
	}
	pins.directoryCreated = true
	directoryInfo, err := m.ops.statPath(pins.directory)
	if err != nil {
		return fail(fmt.Errorf("stat opaque namespace directory: %w", err))
	}
	// Record cleanup identity before validating the security properties. A
	// directory that fails validation is still ours to remove.
	pins.directoryInfo = directoryInfo
	if !directoryInfo.mode.IsDir() ||
		directoryInfo.mode.Perm() != 0o700 ||
		directoryInfo.uid != m.ops.effectiveUID() ||
		directoryInfo.inode == 0 {
		return fail(errors.New(
			"opaque namespace directory is not a private execd-owned 0700 directory",
		))
	}

	netCreated, err := m.ops.createFile(pins.netPath, 0o600)
	if netCreated {
		pins.netTargetCreated = true
	}
	if err != nil {
		return fail(fmt.Errorf("create network namespace pin target: %w", err))
	}
	userCreated, err := m.ops.createFile(pins.userPath, 0o600)
	if userCreated {
		pins.userTargetCreated = true
	}
	if err != nil {
		return fail(fmt.Errorf("create user namespace pin target: %w", err))
	}
	if err := m.ops.bindMountFD(pins.netFD, pins.netPath); err != nil {
		return fail(fmt.Errorf("bind-pin network namespace: %w", err))
	}
	pins.netMounted = true
	if err := verifyPinnedNamespace(
		m.ops,
		pins.netPath,
		pins.netFD,
		cloneNewNet,
	); err != nil {
		return fail(fmt.Errorf("verify network namespace pin: %w", err))
	}

	if err := m.ops.bindMountFD(pins.userFD, pins.userPath); err != nil {
		return fail(fmt.Errorf("bind-pin owning user namespace: %w", err))
	}
	pins.userMounted = true
	if err := verifyPinnedNamespace(
		m.ops,
		pins.userPath,
		pins.userFD,
		cloneNewUser,
	); err != nil {
		return fail(fmt.Errorf("verify owning user namespace pin: %w", err))
	}
	if err := verifyPinnedOwnership(m.ops, pins.netPath, pins.userFD); err != nil {
		return fail(fmt.Errorf("verify pinned namespace ownership: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	return pins, nil
}

func (m *NamespaceManager) ensureRoot() error {
	if err := m.ops.mkdirAll(m.root, 0o700); err != nil {
		return fmt.Errorf("create namespace root: %w", err)
	}
	info, err := m.ops.statPath(m.root)
	if err != nil {
		return fmt.Errorf("stat namespace root: %w", err)
	}
	if !info.mode.IsDir() ||
		info.mode.Perm() != 0o700 ||
		info.uid != m.ops.effectiveUID() ||
		info.inode == 0 {
		return errors.New(
			"namespace root is not a private execd-owned 0700 directory",
		)
	}
	return nil
}

func acquireNamespaceIdentity(
	ops namespaceOps,
	identity isolation.WorkloadIdentity,
) (_ NamespaceIdentity, netFD int, userFD int, err error) {
	if identity.PID <= 0 ||
		identity.SandboxPID <= 0 ||
		identity.NetNamespaceID == 0 ||
		identity.ProcessStartTimeTicks == 0 {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"%w: pid=%d sandbox_pid=%d net_inode=%d starttime=%d",
			ErrInvalidIdentity,
			identity.PID,
			identity.SandboxPID,
			identity.NetNamespaceID,
			identity.ProcessStartTimeTicks,
		)
	}

	startTime, err := readProcessStartTime(ops, identity.PID)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"read authenticated workload starttime: %w",
			err,
		)
	}
	if startTime != identity.ProcessStartTimeTicks {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"%w: pid=%d authenticated=%d proc=%d",
			ErrPIDReused,
			identity.PID,
			identity.ProcessStartTimeTicks,
			startTime,
		)
	}

	netPath := filepath.Join("/proc", strconv.Itoa(identity.PID), "ns", "net")
	netFD, err = ops.openNamespace(netPath)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"open authenticated workload network namespace: %w",
			err,
		)
	}
	openedNetFD := netFD
	closeNet := true
	defer func() {
		if closeNet {
			_ = ops.closeFD(openedNetFD)
		}
	}()
	netInfo, err := validateNamespaceFD(ops, netFD, cloneNewNet)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"validate authenticated workload network namespace: %w",
			err,
		)
	}
	if netInfo.inode != identity.NetNamespaceID {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"%w: authenticated=%d proc=%d",
			ErrNamespaceMismatch,
			identity.NetNamespaceID,
			netInfo.inode,
		)
	}

	userFD, err = ops.namespaceOwner(netFD)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"get network namespace owning user namespace: %w",
			err,
		)
	}
	openedUserFD := userFD
	closeUser := true
	defer func() {
		if closeUser {
			_ = ops.closeFD(openedUserFD)
		}
	}()
	userInfo, err := validateNamespaceFD(ops, userFD, cloneNewUser)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"validate network namespace owning user namespace: %w",
			err,
		)
	}
	ownerUID, err := resolveNamespaceOwnerUID(ops, userFD)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, err
	}

	// Re-read the authenticated process and namespace after NS_GET_USERNS.
	// The already-open descriptors cannot be redirected, and this second
	// snapshot proves the numeric PID did not disappear and get recycled.
	startTime, err = readProcessStartTime(ops, identity.PID)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"re-read authenticated workload starttime: %w",
			err,
		)
	}
	if startTime != identity.ProcessStartTimeTicks {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"%w: pid=%d authenticated=%d proc=%d",
			ErrPIDReused,
			identity.PID,
			identity.ProcessStartTimeTicks,
			startTime,
		)
	}
	currentNetFD, err := ops.openNamespace(netPath)
	if err != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"re-open authenticated workload network namespace: %w",
			err,
		)
	}
	currentNetInfo, validateErr := validateNamespaceFD(
		ops,
		currentNetFD,
		cloneNewNet,
	)
	closeErr := ops.closeFD(currentNetFD)
	if validateErr != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"re-validate authenticated workload network namespace: %w",
			validateErr,
		)
	}
	if closeErr != nil {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"close authenticated workload verification descriptor: %w",
			closeErr,
		)
	}
	if !sameObject(netInfo, currentNetInfo) {
		return NamespaceIdentity{}, -1, -1, fmt.Errorf(
			"%w: pid=%d network namespace changed during acquisition",
			ErrPIDReused,
			identity.PID,
		)
	}

	closeNet = false
	closeUser = false
	return NamespaceIdentity{
		PID:                   identity.PID,
		ProcessStartTimeTicks: identity.ProcessStartTimeTicks,
		NetDevice:             netInfo.dev,
		NetInode:              netInfo.inode,
		OwningUserDevice:      userInfo.dev,
		OwningUserInode:       userInfo.inode,
		OwningUserUID:         ownerUID,
	}, netFD, userFD, nil
}

func resolveNamespaceOwnerUID(ops namespaceOps, userFD int) (uint32, error) {
	ownerUID, ownerErr := ops.namespaceOwnerUID(userFD)
	if ownerErr == nil {
		return ownerUID, nil
	}
	if !errors.Is(ownerErr, syscall.EPERM) {
		return 0, fmt.Errorf(
			"get network namespace owning user namespace uid: %w",
			ownerErr,
		)
	}

	// NS_GET_OWNER_UID is not defined for the initial user namespace. Legacy
	// private-network sessions using setpriv still need stable pins, so accept
	// execd's effective UID only after proving the returned owner is exactly
	// execd's current user namespace. Any other ioctl failure remains fatal.
	currentUserFD, err := ops.openNamespace("/proc/self/ns/user")
	if err != nil {
		return 0, fmt.Errorf(
			"get network namespace owning user namespace uid: %w",
			ownerErr,
		)
	}
	currentUser, validateErr := validateNamespaceFD(
		ops,
		currentUserFD,
		cloneNewUser,
	)
	expectedUser, expectedErr := validateNamespaceFD(
		ops,
		userFD,
		cloneNewUser,
	)
	closeErr := ops.closeFD(currentUserFD)
	if validateErr != nil {
		return 0, fmt.Errorf("validate current user namespace: %w", validateErr)
	}
	if expectedErr != nil {
		return 0, expectedErr
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close current user namespace descriptor: %w", closeErr)
	}
	if !sameObject(currentUser, expectedUser) {
		return 0, fmt.Errorf(
			"get network namespace owning user namespace uid: %w",
			ownerErr,
		)
	}
	return ops.effectiveUID(), nil
}

func validateNamespaceFD(
	ops namespaceOps,
	fd int,
	expectedType int,
) (pathInfo, error) {
	info, err := ops.statFD(fd)
	if err != nil {
		return pathInfo{}, err
	}
	if !info.mode.IsRegular() || info.dev == 0 || info.inode == 0 {
		return pathInfo{}, errors.New("namespace descriptor is not a regular nsfs file")
	}
	fsType, err := ops.fileSystemType(fd)
	if err != nil {
		return pathInfo{}, err
	}
	if fsType != nsfsSuperMagic {
		return pathInfo{}, fmt.Errorf(
			"namespace descriptor filesystem type=%#x, want nsfs",
			fsType,
		)
	}
	namespaceType, err := ops.namespaceType(fd)
	if err != nil {
		return pathInfo{}, err
	}
	if namespaceType != expectedType {
		return pathInfo{}, fmt.Errorf(
			"namespace descriptor type=%#x, want %#x",
			namespaceType,
			expectedType,
		)
	}
	return info, nil
}

func verifyPinnedNamespace(
	ops namespaceOps,
	path string,
	sourceFD int,
	expectedType int,
) error {
	expected, err := validateNamespaceFD(ops, sourceFD, expectedType)
	if err != nil {
		return fmt.Errorf("validate source descriptor: %w", err)
	}
	pinnedFD, err := ops.openNamespace(path)
	if err != nil {
		return err
	}
	actual, validateErr := validateNamespaceFD(ops, pinnedFD, expectedType)
	closeErr := ops.closeFD(pinnedFD)
	if validateErr != nil {
		return validateErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !sameObject(expected, actual) {
		return fmt.Errorf(
			"pinned namespace identity changed: source=%d:%d pinned=%d:%d",
			expected.dev,
			expected.inode,
			actual.dev,
			actual.inode,
		)
	}
	return nil
}

func verifyPinnedOwnership(
	ops namespaceOps,
	netPath string,
	expectedUserFD int,
) error {
	expectedOwner, err := validateNamespaceFD(ops, expectedUserFD, cloneNewUser)
	if err != nil {
		return err
	}
	netFD, err := ops.openNamespace(netPath)
	if err != nil {
		return err
	}
	ownerFD, ownerErr := ops.namespaceOwner(netFD)
	closeNetErr := ops.closeFD(netFD)
	if ownerErr != nil {
		return ownerErr
	}
	actualOwner, validateErr := validateNamespaceFD(ops, ownerFD, cloneNewUser)
	closeOwnerErr := ops.closeFD(ownerFD)
	if closeNetErr != nil {
		return closeNetErr
	}
	if validateErr != nil {
		return validateErr
	}
	if closeOwnerErr != nil {
		return closeOwnerErr
	}
	if !sameObject(expectedOwner, actualOwner) {
		return fmt.Errorf(
			"pinned network namespace owner changed: expected=%d:%d actual=%d:%d",
			expectedOwner.dev,
			expectedOwner.inode,
			actualOwner.dev,
			actualOwner.inode,
		)
	}
	return nil
}

func readProcessStartTime(ops namespaceOps, pid int) (uint64, error) {
	data, err := ops.readFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	// proc(5) permits spaces and ')' characters in comm. Field 3 begins after
	// the final ')' and starttime is field 22, index 19 in that suffix.
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return 0, errors.New("malformed process stat")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) <= 19 {
		return 0, errors.New("process stat is missing starttime")
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTime == 0 {
		return 0, fmt.Errorf("invalid process starttime %q", fields[19])
	}
	return startTime, nil
}

func randomNamespaceID(source io.Reader) (string, error) {
	raw := make([]byte, namespaceIDBytes)
	if _, err := io.ReadFull(source, raw); err != nil {
		return "", fmt.Errorf("generate namespace identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Close releases both bind mounts, removes their private directory, and closes
// the original namespace descriptors. It never follows or removes a directory
// whose recorded identity has changed.
func (p *NamespacePins) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}

	if err := p.validateDirectoryForCleanup(); err != nil {
		return errors.Join(ErrCleanupIncomplete, err)
	}
	if err := errors.Join(
		p.unmountPin(
			p.netPath,
			"network namespace pin",
			&p.netMounted,
		),
		p.unmountPin(
			p.userPath,
			"owning user namespace pin",
			&p.userMounted,
		),
	); err != nil {
		return errors.Join(ErrCleanupIncomplete, err)
	}
	if err := errors.Join(
		p.removePinTarget(
			p.netPath,
			"network namespace pin target",
			&p.netTargetCreated,
		),
		p.removePinTarget(
			p.userPath,
			"user namespace pin target",
			&p.userTargetCreated,
		),
	); err != nil {
		return errors.Join(ErrCleanupIncomplete, err)
	}
	if err := p.removeDirectory(); err != nil {
		return errors.Join(ErrCleanupIncomplete, err)
	}

	cleanupErr := errors.Join(
		p.closeDescriptor(
			"network namespace descriptor",
			&p.netFD,
		),
		p.closeDescriptor(
			"owning user namespace descriptor",
			&p.userFD,
		),
	)
	p.closed = true
	return cleanupErr
}

func (p *NamespacePins) validateDirectoryForCleanup() error {
	if !p.directoryCreated {
		return nil
	}

	current, err := p.ops.statPath(p.directory)
	if err != nil {
		return fmt.Errorf("stat namespace directory before cleanup: %w", err)
	}
	if !current.mode.IsDir() ||
		current.uid != p.ops.effectiveUID() ||
		(p.directoryInfo.inode != 0 &&
			!sameObject(current, p.directoryInfo)) {
		return errors.New("namespace directory identity changed before cleanup")
	}
	if p.directoryInfo.inode != 0 {
		return nil
	}

	// mkdir succeeded but its first stat failed. At that point Pin has not
	// created any children yet, so it is safe within the trusted execd-owned
	// root to adopt the now-verifiable directory identity and finish rollback.
	// Never adopt a directory after later resource creation, because its
	// contents could belong to another object.
	if p.netTargetCreated ||
		p.userTargetCreated ||
		p.netMounted ||
		p.userMounted {
		return errors.New(
			"namespace directory identity was unavailable after resource creation",
		)
	}
	p.directoryInfo = current
	return nil
}

func (p *NamespacePins) unmountPin(
	path string,
	description string,
	mounted *bool,
) error {
	if !*mounted {
		return nil
	}
	if err := p.ops.unmount(path); err != nil &&
		!errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("unmount %s: %w", description, err)
	}
	*mounted = false
	return nil
}

func (p *NamespacePins) removePinTarget(
	path string,
	description string,
	created *bool,
) error {
	if !*created {
		return nil
	}
	if err := p.ops.remove(path); err != nil &&
		!errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", description, err)
	}
	*created = false
	return nil
}

func (p *NamespacePins) removeDirectory() error {
	if !p.directoryCreated {
		return nil
	}
	if err := p.ops.remove(p.directory); err != nil &&
		!errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove namespace directory: %w", err)
	}
	p.directoryCreated = false
	return nil
}

func (p *NamespacePins) closeDescriptor(
	description string,
	fd *int,
) error {
	if *fd < 0 {
		return nil
	}
	err := p.ops.closeFD(*fd)
	*fd = -1
	if err != nil {
		return fmt.Errorf("close %s: %w", description, err)
	}
	return nil
}
