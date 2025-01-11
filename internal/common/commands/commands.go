// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	k8serror "k8s.io/apimachinery/pkg/api/errors"
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
	// Use the host's dmsetup
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
	// Use the host's lvm
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

// Run a command with nbd-client.
func nbdClient(args ...string) (Output, error) {
	log.Printf("nbd-client command: %v", args)
	// nbd-client lives in our container, so sharing --mount (part
	// of --all) would cause ENOENT (if the host has not installed
	// nbd) or other problems (if the host version lacks -i). But
	// netlink sockets may cause EACCES if we don't share --net.
	return RunInContainer(append([]string{"nsenter", "--target", "1", "--net", "nbd-client"}, args...)...)
}

var (
	nbdClientURIPattern       = regexp.MustCompile(`nbd://([^/]*)/(\S*)`)
	nbdClientConnectedPattern = regexp.MustCompile(`Connected /dev/(\S*)`)
)

// Connect to the NBD server at uri, and return the nbdX device that was
// allocated. For safety, /sys/block/nbdX/backend will contain uri.
func NBDClientConnect(uri string) (string, error) {
	// nbd-client doesn't take URIs, so we have to break up the input
	matches := nbdClientURIPattern.FindStringSubmatch(uri)
	if matches == nil {
		return "", k8serror.NewBadRequest("could not parse NBD URI")
	}

	output, err := nbdClient("--identifier", uri, "--connections", "8", "--name", matches[2], matches[1])
	if err != nil {
		return "", err
	}

	match := nbdClientConnectedPattern.FindSubmatch(output.Combined)
	if match == nil {
		log.Printf("nbd-client output: %s", output.Combined)
		return "", k8serror.NewBadRequest("could not parse NBD device from nbd-client")
	}

	return string(match[1]), nil
}

// Return the backend uri associated with the NBD client device, or the
// empty string if it does not appear to be a kubesan nbd client.
func NBDClientBackend(device string) (string, error) {
	output, err := RunOnHost("cat", "/sys/block/"+device+"/backend")
	if err != nil {
		return "", err
	}

	return strings.TrimSuffix(string(output.Combined), "\n"), nil
}

// Disconnect the NBD client device, if it is still associated with the
// given backend uri (if the device is already disconnected or is associated
// with a different uri, this command is a safe no-op).
func NBDClientDisconnect(uri, device string) error {
	// By itself, this function has a TOCTTOU race. But as long as
	// the caller uses a mutex around this function, we know that
	// no other kubesan thread can win the race between our
	// backend check and the actual disconnect call.
	if backend, err := NBDClientBackend(device); err != nil || backend != uri {
		return err
	}
	// TODO: Once the kernel and nbd-client support it, we should pass
	// --identifier uri to the disconnect call for even more safety.
	// See https://gitlab.com/kubesan/kubesan/-/issues/88
	_, err := nbdClient("--disconnect", "/dev/"+device)
	return err
}
