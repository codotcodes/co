package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func defaultConfigPath() (string, error) {
	root := os.Getenv("APPDATA")
	if root == "" {
		return "", errors.New("APPDATA is unset; set CO_CONFIG_DIR or XDG_CONFIG_HOME for co")
	}
	return filepath.Join(root, "co", "config.json"), nil
}

type configSecurity struct {
	user       *windows.Tokenuser
	descriptor *windows.SECURITY_DESCRIPTOR
}

func newConfigSecurity() (*configSecurity, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	return &configSecurity{user, descriptor}, nil
}

func (security *configSecurity) attributes() *windows.SecurityAttributes {
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: security.descriptor}
}

func (security *configSecurity) open(path string, access, disposition uint32) (*os.File, error) {
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(wide, access|windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, security.attributes(), disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	runtime.KeepAlive(security)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fail(err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(errors.New("config cannot be a reparse point"))
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fail(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fail(err)
	}
	if owner == nil || !owner.Equals(security.user.User.Sid) {
		return fail(errors.New("config is owned by another Windows identity"))
	}
	dacl, _, err := security.descriptor.DACL()
	if err != nil {
		return fail(err)
	}
	if dacl == nil {
		return fail(errors.New("missing private config DACL"))
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fail(err)
	}
	runtime.KeepAlive(security)
	return file, nil
}

func secureConfigDirectory(path string) error {
	security, err := newConfigSecurity()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		wide, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		err = windows.CreateDirectory(wide, security.attributes())
		runtime.KeepAlive(security)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
	}
	file, err := security.open(path, 0, windows.OPEN_EXISTING)
	if err != nil {
		return err
	}
	return file.Close()
}

func readPrivateConfig(path string) ([]byte, error) {
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		if err := secureConfigDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	security, err := newConfigSecurity()
	if err != nil {
		return nil, err
	}
	file, err := security.open(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func openConfigLock(path string) (*os.File, error) {
	security, err := newConfigSecurity()
	if err != nil {
		return nil, err
	}
	return security.open(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
}
func lockConfig(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, ^uint32(0), ^uint32(0), &windows.Overlapped{})
}
func unlockConfig(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, ^uint32(0), ^uint32(0), &windows.Overlapped{})
}

func createConfigTemp(parent string) (*os.File, error) {
	security, err := newConfigSecurity()
	if err != nil {
		return nil, err
	}
	for range 10 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		file, err := security.open(filepath.Join(parent, "config.json."+hex.EncodeToString(nonce[:])+".tmp"), windows.GENERIC_WRITE, windows.CREATE_NEW)
		if !errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return file, err
		}
	}
	return nil, errors.New("unable to reserve private config temporary file")
}

func replaceConfig(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
