package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
)

func main() {
	r := Runner{}
	flag.StringVar(&r.TestCommand, "test", "yarn test", "The command that evaluates if an update works")
	flag.StringVar(&r.RootDir, "c", ".", "The root directory of the module to update")
	flag.BoolVar(&r.DoCommit, "commit", false, "Commit changes")
	flag.BoolVar(&r.Verbose, "v", false, "Show output of test runs")
	flag.Parse()

	if err := r.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

// Runner holds the state for an update run
type Runner struct {
	RootDir             string
	TestCommand         string
	DoCommit            bool
	Verbose             bool
	OriginalPackageJSON *PackageJSON
}

func (r *Runner) Run() error {
	cmd := exec.Command("yarn", "install")
	cmd.Dir = r.RootDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	var err error
	r.OriginalPackageJSON, err = r.readPackageJSON()
	if err != nil {
		return err
	}

	updates, err := r.getUpdates()
	if err != nil {
		return err
	}

	initialTestPassed, err := r.test()
	if err != nil {
		return err
	}
	if !initialTestPassed {
		fmt.Printf("%s\n", color.RedString("test failed before upgrading anything, aborting."))
		return nil
	}

	goodUpdates, err := r.try(updates, "")
	if err != nil {
		_ = r.writePackageJSON(r.OriginalPackageJSON)
		return err
	}

	// rewrite the mod file with the updated packages
	mod := copyMod(r.OriginalPackageJSON)
	setVersions(mod, goodUpdates)
	if err := r.writePackageJSON(mod); err != nil {
		_ = r.writePackageJSON(r.OriginalPackageJSON)
		return err
	}

	finalTestPassed, err := r.test()
	if err != nil {
		return err
	}
	if !finalTestPassed {
		fmt.Printf("%s\n", color.RedString("test failed after applying upgrades, aborting."))
		return nil
	}

	for _, req := range goodUpdates {
		fmt.Printf("%s: %s %s -> %s\n", color.GreenString("package upgraded"),
			req.Package, req.Current, req.Latest)
	}
	for _, req := range updates {
		if !inUpdates(goodUpdates, req.Package) {
			fmt.Printf("%s: %s %s -> %s\n", color.RedString("package upgrade failed"),
				req.Package, req.Current, req.Latest)
		}
	}

	if r.DoCommit && len(goodUpdates) > 0 {
		message := []string{"Update package.json", ""}
		for _, req := range goodUpdates {
				message = append(message, fmt.Sprintf("* upgrade %s from %s to %s",
					req.Package, req.Current, req.Latest))
			}
		cmd := exec.Command("git", "-C", r.RootDir, "add", "-A")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = r.RootDir
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git add failed: %v", err)
		}
		cmd = exec.Command("git", "commit", "-m", strings.Join(message, "\n"))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = r.RootDir
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git commit failed: %v", err)
		}
	}

	return nil
}

// try tries to apply `updates` by performing the update and running the test. If the
// tests fail, it invokes itself recursively with a smaller set of updates. Returns a list of
// the updates that passed the test.
func (r Runner) try(updates []Update, indent string) ([]Update, error) {
	fmt.Printf("%strying %d updates\n", indent, len(updates))
	for _, req := range updates {
		fmt.Printf("%s  %s: %s -> %s\n", indent, req.Package, req.Current, req.Latest)
	}

	if len(updates) == 0 {
		return nil, nil
	}

	mod := copyMod(r.OriginalPackageJSON)
	setVersions(mod, updates)
	err := r.writePackageJSON(mod)
	if err != nil {
		return nil, err
	}

	fmt.Printf("%s  yarn install\n", indent)
	cmd := exec.Command("yarn", "install")
	cmd.Dir = r.RootDir
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	ok, err := r.test()
	if err != nil {
		return nil, err
	}
	if ok {
		fmt.Printf("%s  test passed\n", indent)
		return updates, nil
	}

	fmt.Printf("%s  test failed\n", indent)

	// if we are testing only one package, and it fails, then this package
	// is bad, and we shouldn't include it in the update
	if len(updates) == 1 {
		return []Update{}, nil
	}

	// more than one package was being updated, so we split the updates in half
	// and try them separately, to see if we can figure out which ones are actually
	// broken
	requireA, requireB := bisect(updates)

	successA, err := r.try(requireA, indent + "  ")
	if err != nil {
		return nil, err
	}
	successB, err := r.try(requireB, indent + "  ")
	if err != nil {
		return nil, err
	}

	goodUpdates := append(successA, successB...)
	fmt.Printf("%skeeping %d of %d updates:\n", indent, len(goodUpdates), len(updates))
	for _, req := range goodUpdates {
		fmt.Printf("%s  %s: %s -> %s\n", indent, req.Package, req.Current, req.Latest)
	}

	return goodUpdates, nil
}

// test runs the tests to determine if an upgrade was successful
func (r Runner) test() (bool, error) {
	log.Printf("running test: %s", r.TestCommand)
	cmd := exec.Command("/bin/sh", "-c", r.TestCommand)
	cmd.Dir = r.RootDir
	if r.Verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot run test program: %s", err)
	}
	return true, nil
}

type Update struct {
	Package string
	Current string
	Wanted string
	Latest string
}

func (r Runner) getUpdates() ([]Update, error) {
	log.Printf("running npm outdated")
	cmd := exec.Command("npm", "outdated")
	cmd.Stderr = os.Stderr
	cmd.Dir = r.RootDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var updates []Update

	stdoutScanner := bufio.NewScanner(stdout)
	stdoutScanner.Scan()  // first line is a header
	for stdoutScanner.Scan() {
		fmt.Println(stdoutScanner.Text())
		parts := strings.Fields(stdoutScanner.Text())
		update := Update{
			Package: parts[0],
			Current: parts[1],
			Wanted: parts[2],
			Latest: parts[3],
		}
		updates = append(updates, update)
	}
	if err := stdoutScanner.Err(); err!= nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if (exitErr.ExitCode() == 1) {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return updates, nil
}

type PackageJSON struct {
	raw json.RawMessage
	Dependencies map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// readPackageJSON reads and parses package.json
func (r Runner) readPackageJSON() (*PackageJSON, error) {
	buf, err := ioutil.ReadFile(filepath.Join(r.RootDir, "package.json"))
	if err != nil {
		return nil, err
	}

	rv := PackageJSON{}
	if err := json.Unmarshal(buf, &rv); err != nil {
		return nil, err
	}
	rv.raw = buf

	return &rv, nil
}

// writePackageJSON writes `mf` to package.json.
func (r Runner) writePackageJSON(mf *PackageJSON) (error) {
	var m map[string]interface{}
	if err := json.Unmarshal(mf.raw, &m); err != nil {
		return err
	}
	m["dependencies"] = mf.Dependencies
	m["devDependencies"] = mf.DevDependencies

	buf, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(r.RootDir, "package.json"), buf, 0644)
}


// bisect returns two require lists, each containing approximately half of the
// items in `updates`
func bisect(updates []Update) ([]Update, []Update) {
	a, b := []Update{}, []Update{}
	for i := range updates {
		if i % 2 == 0 {
			a = append(a, updates[i])
		} else {
			b = append(b, updates[i])
		}
	}
	return a,b
}

// setVersions updates the requirements in `mf` with the updates described
// by `updates`.
func setVersions(mf *PackageJSON, updates []Update) {
	for _, req := range updates {
		_, ok := mf.DevDependencies[req.Package]
		if ok {
			mf.DevDependencies[req.Package] = req.Latest
		} else {
			mf.Dependencies[req.Package] = req.Latest
		}
	}
}

// copyMod returns a copy of `mf` by serializing and re-parsing it.
func copyMod(mf *PackageJSON) *PackageJSON {
	copy := PackageJSON{
		raw: mf.raw,
		Dependencies: map[string]string{},
		DevDependencies: map[string]string{},
	}
	for k,v := range mf.Dependencies {
		copy.Dependencies[k] = v
	}
	for k,v := range mf.DevDependencies {
		copy.DevDependencies[k] = v
	}
	return &copy
}

func inUpdates(updates []Update, pkg string) bool {
	for _, update := range updates {
		if update.Package == pkg {
			return true
		}
	}
	return false
}

