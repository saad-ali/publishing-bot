package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/golang/glog"
	yaml "gopkg.in/yaml.v2"

	"strings"

	"k8s.io/publishing-bot/cmd/publishing-bot/config"
)

const (
	depCommit        = "7c44971bbb9f0ed87db40b601f2d9fe4dffb750d"
	godepCommit      = "tags/v80"
	DefaultGoVersion = "1.10.2"
)

var (
	SystemGoPath = os.Getenv("GOPATH")
	BaseRepoPath = filepath.Join(SystemGoPath, "src", "k8s.io")
)

func Usage() {
	fmt.Fprintf(os.Stderr, `
Usage: %s [-config <config-yaml-file>] [-source-repo <repo>] [-source-org <org>] [-rules-file <file> ] [-skip-godep|skip-dep] [-target-org <org>]

Command line flags override config values.
`, os.Args[0])
	flag.PrintDefaults()
}

func main() {
	configFilePath := flag.String("config", "", "the config file in yaml format")
	githubHost := flag.String("github-host", "", "the address of github (defaults to github.com)")
	basePackage := flag.String("base-package", "", "the name of the package base (defaults to k8s.io when source repo is kubernetes, "+
		"otherwise github-host/target-org)")
	repoName := flag.String("source-repo", "", "the name of the source repository (eg. kubernetes)")
	repoOrg := flag.String("source-org", "", "the name of the source repository organization, (eg. kubernetes)")
	rulesFile := flag.String("rules-file", "", "the file with repository rules")
	targetOrg := flag.String("target-org", "", `the target organization to publish into (e.g. "k8s-publishing-bot")`)
	skipGodep := flag.Bool("skip-godep", false, `skip godeps installation and godeps-restore`)
	skipDep := flag.Bool("skip-dep", false, `skip 'dep'' installation`)

	flag.Usage = Usage
	flag.Parse()

	cfg := config.Config{}
	if *configFilePath != "" {
		bs, err := ioutil.ReadFile(*configFilePath)
		if err != nil {
			glog.Fatalf("Failed to load config file from %q: %v", *configFilePath, err)
		}
		if err := yaml.Unmarshal(bs, &cfg); err != nil {
			glog.Fatalf("Failed to parse config file at %q: %v", *configFilePath, err)
		}
	}

	if *targetOrg != "" {
		cfg.TargetOrg = *targetOrg
	}
	if *repoName != "" {
		cfg.SourceRepo = *repoName
	}
	if *repoOrg != "" {
		cfg.SourceOrg = *repoOrg
	}
	if *githubHost != "" {
		cfg.GithubHost = *githubHost
	}
	if *basePackage != "" {
		cfg.BasePackage = *basePackage
	}

	if cfg.GithubHost == "" {
		cfg.GithubHost = "github.com"
	}
	// defaulting when base package is not specified
	if cfg.BasePackage == "" {
		if cfg.SourceRepo == "kubernetes" {
			cfg.BasePackage = "k8s.io"
		} else {
			cfg.BasePackage = filepath.Join(cfg.GithubHost, cfg.TargetOrg)
		}
	}

	BaseRepoPath = filepath.Join(SystemGoPath, "src", cfg.BasePackage)

	if *rulesFile != "" {
		cfg.RulesFile = *rulesFile
	}

	if len(cfg.SourceRepo) == 0 || len(cfg.SourceOrg) == 0 {
		glog.Fatalf("source-org and source-repo cannot be empty")
	}

	if len(cfg.TargetOrg) == 0 {
		glog.Fatalf("Target organization cannot be empty")
	}

	// If RULE_FILE_PATH is detected, check if the source repository include rules files.
	if len(os.Getenv("RULE_FILE_PATH")) > 0 {
		cfg.RulesFile = filepath.Join(BaseRepoPath, cfg.SourceRepo, os.Getenv("RULE_FILE_PATH"))
	}

	if len(cfg.RulesFile) == 0 {
		glog.Fatalf("No rules file provided")
	}
	rules, err := config.LoadRules(cfg.RulesFile)
	if err != nil {
		glog.Fatalf("Failed to load rules: %v", err)
	}

	goVersions := []string{DefaultGoVersion}
	for _, rule := range rules.Rules {
		for _, branch := range rule.Branches {
			if branch.GoVersion != "" {
				found := false
				for _, v := range goVersions {
					if v == branch.GoVersion {
						found = true
					}
				}
				if !found {
					goVersions = append(goVersions, branch.GoVersion)
				}
			}
		}
	}
	for _, v := range goVersions {
		installGoVersion(v, filepath.Join(SystemGoPath, "go-"+v))
	}
	goLink, target := filepath.Join(SystemGoPath, "go"), filepath.Join(SystemGoPath, "go-"+DefaultGoVersion)
	os.Remove(goLink)
	if err := os.Symlink(target, goLink); err != nil {
		glog.Fatalf("Failed to link %s to %s: %s", goLink, target, err)
	}

	if err := os.MkdirAll(BaseRepoPath, os.ModePerm); err != nil {
		glog.Fatalf("Failed to create source repo directory %s: %v", BaseRepoPath, err)
	}

	if !*skipGodep {
		installGodeps()
	}
	if !*skipDep {
		installDep()
	}

	cloneSourceRepo(cfg, *skipGodep)
	for _, rule := range rules.Rules {
		cloneForkRepo(cfg, rule.DestinationRepository)
	}
}

func installGoVersion(v string, pth string) {
	if s, err := os.Stat(pth); err != nil && !os.IsNotExist(err) {
		glog.Fatal(err)
	} else if err == nil {
		if s.IsDir() {
			glog.Infof("Found existing go %s at %s", v, pth)
			return
		}
		glog.Fatalf("Expected %s to be a directory", pth)
	}

	glog.Infof("Installing go %s to %s", v, pth)
	tmpPath, err := ioutil.TempDir(SystemGoPath, "go-tmp-")
	if err != nil {
		glog.Fatal(err)
	}
	defer os.RemoveAll(tmpPath)
	cmd := exec.Command("/bin/bash", "-c", fmt.Sprintf("curl -SLf https://storage.googleapis.com/golang/go%s.linux-amd64.tar.gz | tar -xz --strip 1 -C %s", v, tmpPath))
	cmd.Dir = tmpPath
	run(cmd)
	if err := os.Rename(tmpPath, pth); err != nil {
		glog.Fatal(err)
	}
}

func cloneForkRepo(cfg config.Config, repoName string) {
	forkRepoLocation := fmt.Sprintf("https://%s/%s/%s", cfg.GithubHost, cfg.TargetOrg, repoName)
	repoDir := filepath.Join(BaseRepoPath, repoName)

	if _, err := os.Stat(repoDir); err == nil {
		glog.Infof("Fork repository %q already cloned to %s, resetting remote URL ...", repoName, repoDir)
		setUrlCmd := exec.Command("git", "remote", "set-url", "origin", forkRepoLocation)
		setUrlCmd.Dir = repoDir
		run(setUrlCmd)
		os.Remove(filepath.Join(repoDir, ".git", "index.lock"))
		return
	}

	glog.Infof("Cloning fork repository %s ...", forkRepoLocation)
	run(exec.Command("git", "clone", forkRepoLocation))

	// TODO: This can be set as an env variable for the container
	setUsernameCmd := exec.Command("git", "config", "user.name", os.Getenv("GIT_COMMITTER_NAME"))
	setUsernameCmd.Dir = repoDir
	run(setUsernameCmd)

	// TODO: This can be set as an env variable for the container
	setEmailCmd := exec.Command("git", "config", "user.email", os.Getenv("GIT_COMMITTER_EMAIL"))
	setEmailCmd.Dir = repoDir
	run(setEmailCmd)
}

func installGodeps() {
	if _, err := exec.LookPath("godep"); err == nil {
		glog.Infof("Already installed: godep")
		return
	}
	glog.Infof("Installing github.com/tools/godep#%s ...", godepCommit)
	run(exec.Command("go", "get", "github.com/tools/godep"))

	godepDir := filepath.Join(SystemGoPath, "src", "github.com", "tools", "godep")
	godepCheckoutCmd := exec.Command("git", "checkout", godepCommit)
	godepCheckoutCmd.Dir = godepDir
	run(godepCheckoutCmd)

	godepInstallCmd := exec.Command("go", "install", "./...")
	godepInstallCmd.Dir = godepDir
	run(godepInstallCmd)
}

func installDep() {
	if _, err := exec.LookPath("dep"); err == nil {
		glog.Infof("Already installed: dep")
		return
	}
	glog.Infof("Installing github.com/golang/dep#%s ...", depCommit)
	depGoGetCmd := exec.Command("go", "get", "github.com/golang/dep")
	run(depGoGetCmd)

	depDir := filepath.Join(SystemGoPath, "src", "github.com", "golang", "dep")
	depCheckoutCmd := exec.Command("git", "checkout", depCommit)
	depCheckoutCmd.Dir = depDir
	run(depCheckoutCmd)

	depInstallCmd := exec.Command("go", "install", "./cmd/dep")
	depInstallCmd.Dir = depDir
	run(depInstallCmd)
}

// run wraps the cmd.Run() command and sets the standard output and common environment variables.
// if the c.Dir is not set, the BaseRepoPath will be used as a base directory for the command.
func run(c *exec.Cmd) {
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if len(c.Dir) == 0 {
		c.Dir = BaseRepoPath
	}
	if err := c.Run(); err != nil {
		glog.Fatalf("Command %q failed: %v", strings.Join(c.Args, " "), err)
	}
}

func cloneSourceRepo(cfg config.Config, runGodepRestore bool) {
	if _, err := os.Stat(filepath.Join(BaseRepoPath, cfg.SourceRepo)); err == nil {
		glog.Infof("Source repository %q already cloned, skipping", cfg.SourceRepo)
		return
	}

	repoLocation := fmt.Sprintf("https://%s/%s/%s", cfg.GithubHost, cfg.SourceOrg, cfg.SourceRepo)
	glog.Infof("Cloning source repository %s ...", repoLocation)
	cloneCmd := exec.Command("git", "clone", repoLocation)
	run(cloneCmd)

	if runGodepRestore {
		glog.Infof("Running hack/godep-restore.sh ...")
		restoreCmd := exec.Command("bash", "-x", "hack/godep-restore.sh")
		restoreCmd.Dir = filepath.Join(BaseRepoPath, cfg.SourceRepo)
		run(restoreCmd)
	}
}
