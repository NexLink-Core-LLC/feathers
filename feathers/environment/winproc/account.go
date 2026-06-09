//go:build windows

package winproc

import (
	"crypto/rand"
	"fmt"
	"strings"
	"unsafe"

	"emperror.dev/errors"
	"golang.org/x/sys/windows"
)

// Per-server isolation runs each game server as its own restricted local
// Windows account (see docs/phase0-conformance-spec.md §7). The account is
// created on demand, added only to the built-in Users group (least privilege +
// the local-logon right needed for CreateProcessAsUser), and its password is
// reset to a fresh random value on every start so no secret is ever persisted.

const (
	// USER_PRIV_USER is a standard (non-admin) user.
	userPrivUser = 1
	// Account control flags: a normal, script-capable account whose password
	// never expires (we rotate it ourselves on each start).
	ufScript          = 0x0001
	ufNormalAccount   = 0x0200
	ufDontExpirePassd = 0x10000

	// NetUserAdd status codes we care about.
	nerrSuccess    = 0
	nerrUserExists = 2224

	// LogonUser parameters.
	logon32LogonInteractive = 2
	logon32ProviderDefault  = 0
)

var (
	modnetapi32           = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd        = modnetapi32.NewProc("NetUserAdd")
	procNetUserDel        = modnetapi32.NewProc("NetUserDel")
	procNetUserSetInfo    = modnetapi32.NewProc("NetUserSetInfo")
	procNetLocalGroupAddM = modnetapi32.NewProc("NetLocalGroupAddMembers")

	modadvapi32   = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUser = modadvapi32.NewProc("LogonUserW")
)

// userInfo1 mirrors USER_INFO_1 for NetUserAdd (level 1).
type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

// userInfo1003 mirrors USER_INFO_1003 for NetUserSetInfo (password reset).
type userInfo1003 struct {
	Password *uint16
}

// localGroupMembersInfo3 mirrors LOCALGROUP_MEMBERS_INFO_3 (member by name).
type localGroupMembersInfo3 struct {
	DomainAndName *uint16
}

// accountName derives a valid (<=20 char) local username from a server UUID,
// e.g. "pt_1a2b3c4d".
func accountName(serverID string) string {
	id := strings.ReplaceAll(serverID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return "pt_" + id
}

// randomPassword returns a 24-character password that satisfies the default
// Windows complexity policy (upper, lower, digit, symbol).
func randomPassword() (string, error) {
	const (
		upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		lower = "abcdefghijklmnopqrstuvwxyz"
		digit = "0123456789"
		sym   = "!@#$%^&*()-_=+"
		all   = upper + lower + digit + sym
	)
	pick := func(set string) (byte, error) {
		b := make([]byte, 1)
		if _, err := rand.Read(b); err != nil {
			return 0, err
		}
		return set[int(b[0])%len(set)], nil
	}
	out := make([]byte, 0, 24)
	// Guarantee one of each class first.
	for _, set := range []string{upper, lower, digit, sym} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < 24 {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	return string(out), nil
}

// ensureAccount creates the per-server local account if it does not exist, sets
// its password to pw, and ensures it is a member of the built-in Users group.
// It is safe to call repeatedly (idempotent).
func ensureAccount(name, pw string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	pwPtr, err := windows.UTF16PtrFromString(pw)
	if err != nil {
		return err
	}

	info := userInfo1{
		Name:     namePtr,
		Password: pwPtr,
		Priv:     userPrivUser,
		Flags:    ufScript | ufNormalAccount | ufDontExpirePassd,
	}
	var parmErr uint32
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&parmErr)))
	switch r {
	case nerrSuccess:
		// created fresh
	case nerrUserExists:
		// Already present (e.g. leftover from a previous run) — just reset the
		// password to our fresh value.
		if err := setAccountPassword(name, pw); err != nil {
			return err
		}
	default:
		return errors.Errorf("winproc: NetUserAdd(%q) failed: status=%d parmErr=%d", name, r, parmErr)
	}

	return addToUsersGroup(name)
}

// setAccountPassword rotates an existing account's password.
func setAccountPassword(name, pw string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	pwPtr, err := windows.UTF16PtrFromString(pw)
	if err != nil {
		return err
	}
	info := userInfo1003{Password: pwPtr}
	var parmErr uint32
	r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(namePtr)), 1003, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&parmErr)))
	if r != nerrSuccess {
		return errors.Errorf("winproc: NetUserSetInfo(%q) failed: status=%d", name, r)
	}
	return nil
}

// addToUsersGroup adds the account to the built-in Users group (granting the
// baseline local-logon right). "Users" is referenced via its well-known SID
// alias to be locale-independent. Already-a-member is not an error.
func addToUsersGroup(name string) error {
	// Resolve the localized name of the built-in Users group from its well-known
	// SID (S-1-5-32-545) so this works on non-English Windows.
	usersSid, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		return errors.Wrap(err, "winproc: could not build Users SID")
	}
	groupName, _, _, err := usersSid.LookupAccount("")
	if err != nil {
		return errors.Wrap(err, "winproc: could not resolve Users group name")
	}
	groupPtr, err := windows.UTF16PtrFromString(groupName)
	if err != nil {
		return err
	}
	memberPtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	member := localGroupMembersInfo3{DomainAndName: memberPtr}
	r, _, _ := procNetLocalGroupAddM.Call(0, uintptr(unsafe.Pointer(groupPtr)), 3, uintptr(unsafe.Pointer(&member)), 1)
	// ERROR_MEMBER_IN_ALIAS (1378) => already a member, which is fine.
	const errorMemberInAlias = 1378
	if r != nerrSuccess && r != errorMemberInAlias {
		return errors.Errorf("winproc: NetLocalGroupAddMembers(Users, %q) failed: status=%d", name, r)
	}
	return nil
}

// logonAccount logs the account in and returns a primary token suitable for
// CreateProcessAsUser. The caller owns the token and must Close it.
func logonAccount(name, pw string) (windows.Token, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	pwPtr, err := windows.UTF16PtrFromString(pw)
	if err != nil {
		return 0, err
	}
	// Empty domain ("." would also work) => local account.
	domainPtr, _ := windows.UTF16PtrFromString(".")

	var token windows.Token
	r, _, e := procLogonUser.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(domainPtr)),
		uintptr(unsafe.Pointer(pwPtr)),
		logon32LogonInteractive,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&token)),
	)
	if r == 0 {
		return 0, errors.Wrap(e, fmt.Sprintf("winproc: LogonUser(%q) failed", name))
	}
	return token, nil
}

// deleteAccount removes the per-server local account. Missing accounts are not
// treated as an error.
func deleteAccount(name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(namePtr)))
	// NERR_UserNotFound (2221) => already gone.
	const nerrUserNotFound = 2221
	if r != nerrSuccess && r != nerrUserNotFound {
		return errors.Errorf("winproc: NetUserDel(%q) failed: status=%d", name, r)
	}
	return nil
}
