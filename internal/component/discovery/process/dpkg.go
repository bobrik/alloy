package process

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// anyExecutable is bits that are set for executable files
const anyExecutable = os.FileMode(0111)

// dpkgFileChecks are used to check file equality, similar to rsync
type dpkgFileChecks struct {
	size int64
	time syscall.Timespec
}

// dpkgFileInfo combines file equality checks with the dpkg version
type dpkgFileInfo struct {
	checks  dpkgFileChecks
	version string
}

type dpkgVersionFinder struct {
	logger log.Logger
	infos  map[string]dpkgFileInfo
}

// newDpkgVersionFinder returns an empty dpkg version finder
func newDpkgVersionFinder(logger log.Logger) *dpkgVersionFinder {
	return &dpkgVersionFinder{logger: logger}
}

// getVersion produces the dpkg package version of the process if it is found in infos
func (f *dpkgVersionFinder) getVersion(pid, exe string) (string, error) {
	expected, ok := f.infos[exe]
	if !ok {
		return "", nil
	}

	actual, err := f.computeDpkgFileChecks(path.Join("/proc", pid, "root", exe))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", err
	}

	if *actual != expected.checks {
		return "", nil
	}

	return expected.version, nil
}

// refresh recomputes mapping from binary paths to their dingFileInfo
func (f *dpkgVersionFinder) refresh() error {
	versions, err := f.packageVersions()
	if err != nil {
		return err
	}

	infos := map[string]dpkgFileInfo{}

	for pkg, version := range versions {
		err = f.populateDpkgPackageInfos(pkg, version, infos)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			level.Error(f.logger).Log("msg", "failed to get process info", "err", err)
		}
	}

	f.infos = infos

	return nil
}

// packageVersions returns a mapping of package names to their installed versions
func (f *dpkgVersionFinder) packageVersions() (map[string]string, error) {
	file, err := os.Open("/var/lib/dpkg/status")
	if err != nil {
		return nil, err
	}

	defer file.Close()

	versions := map[string]string{}

	scanner := bufio.NewScanner(file)

	pkg := ""
	for scanner.Scan() {
		line := scanner.Text()
		if pkg == "" && strings.HasPrefix(line, "Package: ") {
			pkg = strings.TrimPrefix(line, "Package: ")
			continue
		} else if pkg != "" && strings.HasPrefix(line, "Version: ") {
			versions[pkg] = strings.TrimPrefix(line, "Version: ")
			pkg = ""
		}
	}

	return versions, nil
}

// populateDpkgPackageInfos extends infos with the data for the provided package
func (f *dpkgVersionFinder) populateDpkgPackageInfos(pkg, version string, infos map[string]dpkgFileInfo) error {
	file, err := os.Open(fmt.Sprintf("/var/lib/dpkg/info/%s.list", pkg))
	if err != nil {
		return err
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		path := scanner.Text()

		checks, err := f.computeDpkgFileChecks(path)
		if err != nil {
			return err
		}

		if checks == nil {
			continue
		}

		infos[path] = dpkgFileInfo{checks: *checks, version: version}
	}

	return nil
}

// computeDpkgFileChecks computes file equality checks for a single file
func (f *dpkgVersionFinder) computeDpkgFileChecks(path string) (*dpkgFileChecks, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	if info.Mode()&anyExecutable == 0 {
		return nil, nil
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil
	}

	checks := &dpkgFileChecks{
		size: stat.Size,
		time: stat.Mtim,
	}

	return checks, nil
}
