// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

type Output struct {
	ExitCode int
	Combined []byte
}

// If the command exits with a non-zero status, an error is returned alongside the output.
func RunInContainerContext(ctx context.Context, command ...string) (Output, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	cmd.Stdin = nil

	combined, err := cmd.CombinedOutput()

	output := Output{
		Combined: combined,
	}

	switch e := err.(type) {
	case nil:
		output.ExitCode = 0

	case *exec.ExitError:
		output.ExitCode = e.ExitCode()
		err = fmt.Errorf(
			"command \"%s\" failed with exit code %d: %s",
			strings.Join(command, " "), output.ExitCode, combined,
		)
	default:
		output.ExitCode = -1
		err = fmt.Errorf("command \"%s\" failed: %s: %s", strings.Join(command, " "), err, combined)
	}

	return output, err
}

func RunInContainer(command ...string) (Output, error) {
	return RunInContainerContext(context.Background(), command...)
}

func RunOnHostContext(ctx context.Context, command ...string) (Output, error) {
	return RunInContainerContext(ctx, append([]string{"nsenter", "--target", "1", "--all"}, command...)...)
}

func RunOnHost(command ...string) (Output, error) {
	return RunOnHostContext(context.Background(), command...)
}

func PathExistsOnHost(hostPath string) (bool, error) {
	// We run with hostPID: true so we can see the host's root file system
	containerPath := path.Join("/proc/1/root", hostPath)
	_, err := os.Stat(containerPath)
	if err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func Dmsetup(args ...string) (Output, error) {
	log.Printf("dmsetup command: %v", args)
	return RunOnHost(append([]string{"dmsetup"}, args...)...)
}

func DmsetupCreateIdempotent(args ...string) (Output, error) {
	output, err := Dmsetup(append([]string{"create"}, args...)...)

	if err != nil && strings.Contains(string(output.Combined), "resource busy") {
		err = nil // suppress error for idempotency
	}

	return output, err
}

func DmsetupSuspendIdempotent(args ...string) (Output, error) {
	output, err := Dmsetup(append([]string{"suspend"}, args...)...)

	if err != nil && strings.Contains(string(output.Combined), "No such device or address") {
		err = nil // suppress error for idempotency
	}

	return output, err
}

func DmsetupRemoveIdempotent(args ...string) (Output, error) {
	output, err := Dmsetup(append([]string{"remove"}, args...)...)

	if err != nil && strings.Contains(string(output.Combined), "No such device or address") {
		err = nil // suppress error for idempotency
	}

	return output, err
}

// Atomic. Overwrites the profile if it already exists.
func LvmCreateProfile(name string, contents string) error {
	// This should never happen but be extra careful since the name is used to build a path outside the container's
	// mount namespace and container escapes must be prevented.
	if strings.ContainsAny(name, "/") {
		return fmt.Errorf("lvm profile name \"%s\" must not contain a '/' character", name)
	}
	if name == ".." {
		return fmt.Errorf("lvm profile name \"%s\" must not be \"..\"", name)
	}

	// This process runs in the host PID namespace, so the host's root dir is accessible through the init process.
	profileDir := "/proc/1/root/etc/lvm/profile"
	profilePath := path.Join(profileDir, name+".profile")

	f, err := os.CreateTemp(profileDir, "kubesan-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file for lvm profile \"%s\": %s", name, err)
	}
	fClosed := false
	fRenamed := false
	defer func() {
		if !fClosed {
			if err := f.Close(); err != nil {
				panic(fmt.Sprintf("failed to close lvm profile \"%s\": %s", name, err))
			}
		}
		if !fRenamed {
			if err := os.Remove(f.Name()); err != nil {
				panic(fmt.Sprintf("failed to remove temporary file for lvm profile \"%s\": %s", name, err))
			}
		}
	}()

	if err := f.Chmod(0644); err != nil {
		return fmt.Errorf("failed to chmod lvm profile \"%s\": %s", name, err)
	}
	if _, err := f.WriteString(contents); err != nil {
		return fmt.Errorf("failed to write lvm profile \"%s\": %s", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close lvm profile \"%s\": %s", name, err)
	}
	fClosed = true
	if err := os.Rename(f.Name(), profilePath); err != nil {
		return fmt.Errorf("failed to rename lvm profile \"%s\": %s", name, err)
	}
	fRenamed = true
	return nil
}

func Lvm(args ...string) (Output, error) {
	log.Printf("LVM command: %v", args)
	return RunOnHost(append([]string{"lvm"}, args...)...)
}

func LvmLvCreateIdempotent(args ...string) (Output, error) {
	output, err := Lvm(append([]string{"lvcreate"}, args...)...)

	if err != nil && strings.Contains(string(output.Combined), "already exists") {
		err = nil // suppress error for idempotency
	}

	return output, err
}

func LvmLvRemoveIdempotent(args ...string) (Output, error) {
	output, err := Lvm(append([]string{"lvremove"}, args...)...)

	// ignore both "failed to find" and "Failed to find"
	if err != nil && strings.Contains(string(output.Combined), "ailed to find") {
		err = nil // suppress error for idempotency
	}

	return output, err
}

func LvmLvHasTag(vgName string, lvName string, tag string) (bool, error) {
	output, err := Lvm(
		"lvs",
		"--devicesfile", vgName,
		"--select", fmt.Sprintf("lv_tags = {\"%s\"}", tag),
		fmt.Sprintf("%s/%s", vgName, lvName),
	)
	if err != nil {
		return false, err
	}

	// there is output only if the tag is present
	return string(output.Combined) != "", nil
}

func LvmLvAddTag(vgName string, lvName string, tag string) error {
	// lvchange succeeds if the tag is already present
	_, err := Lvm(
		"lvchange",
		"--devicesfile", vgName,
		"--addtag", tag,
		fmt.Sprintf("%s/%s", vgName, lvName),
	)
	return err
}

// Calls a function with an LV activated temporarily
func WithLvmLvActivated(vgName string, lvName string, op func() error) (err error) {
	vgLvName := fmt.Sprintf("%s/%s", vgName, lvName)

	_, err = Lvm("lvchange", "--devicesfile", vgName, "--activate", "y", vgLvName)
	if err != nil {
		return err
	}

	defer func() {
		_, err = Lvm("lvchange", "--devicesfile", vgName, "--activate", "n", vgLvName)
	}()

	return op()
}

var (
	nbdClientConnectedPattern = regexp.MustCompile(`^Connected (/dev/\S*)`)
)

func NBDClientConnect(serverHostname string) (string, error) {
	// we run nbd-client in the host net namespace, so we must resolve the server's hostname here

	serverIps, err := net.LookupIP(serverHostname)
	if err != nil {
		return "", err
	} else if len(serverIps) == 0 {
		return "", fmt.Errorf("could not resolve hostname '%s'", serverHostname)
	}

	output, err := RunOnHost("nbd-client", serverIps[0].String(), "--persist", "--connections", "8")
	if err != nil {
		return "", err
	}

	match := nbdClientConnectedPattern.FindSubmatch(output.Combined)
	if match == nil {
		return "", err
	}

	path := string(match[1])

	return path, nil
}

func NBDClientDisconnect(path string) error {
	_, err := RunOnHost("nbd-client", "-d", path)
	return err
}
