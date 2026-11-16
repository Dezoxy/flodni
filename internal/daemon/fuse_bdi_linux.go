package daemon

import "syscall"

// statBDI returns the BDI id of the filesystem mounted at path.
func statBDI(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return bdiName(uint64(st.Dev)), nil
}
