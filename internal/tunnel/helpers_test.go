package tunnel

import (
	"os/exec"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func execCommandOutput(name string, args ...string) (string, error) {
	b, err := exec.Command(name, args...).Output()
	return string(b), err
}
