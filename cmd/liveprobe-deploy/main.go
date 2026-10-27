// Command liveprobe-deploy is the interactive deploy for LiveProbe (port of scripts/deploy.js):
//   - builds the Docker image locally
//   - uploads the image tarball + compose config straight to the remote directory over SSH
//   - optionally runs `docker compose up -d` on the remote host
//
// Run it from the repo root: go run ./cmd/liveprobe-deploy. Requires docker, ssh and scp
// (local + remote).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	imageName       = "liveprobe"
	composeFile     = "docker-compose.yml"
	collectorConfig = "otel-collector-config.yaml"
	envFile         = ".env"
)

var stdin = bufio.NewReader(os.Stdin)

func prompt(question string) string {
	fmt.Print(question)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func yes(answer string) bool { return strings.HasPrefix(strings.ToLower(answer), "y") }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// run runs a command with live output (stdio inherited).
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// resolveVersion turns the version input into a docker tag suffix (default: latest).
func resolveVersion(input string) (string, error) {
	v := strings.TrimSpace(input)
	if v == "" {
		return "latest", nil
	}
	if strings.ContainsAny(v, ":/") || strings.HasPrefix(v, imageName) {
		return "", fmt.Errorf("enter a version only (e.g. latest, 1.0.0, v1.0.0), not a full tag like %s:1.0.0", imageName)
	}
	return v, nil
}

func imageTag(version string) string { return imageName + ":" + version }

var safeRemoteDir = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// validateRemoteDir rejects remote directories with shell-unsafe characters. Remote paths are
// re-parsed by the server's shell (both `ssh host cmd args` and the `cd <dir>` in runShell), so a
// space or a metacharacter would split the path or inject a command.
func validateRemoteDir(dir string) error {
	if !safeRemoteDir.MatchString(dir) {
		return fmt.Errorf("remote directory %q contains unsupported characters.\nUse only letters, digits, and these: _ @ %% + = : , . / -", dir)
	}
	return nil
}

var (
	sameLineBuild = regexp.MustCompile(`^(\s*)build:\s*\S`)
	blockBuild    = regexp.MustCompile(`^(\s*)build:\s*$`)
)

// prepareRemoteCompose strips build: blocks and pins image tags for the remote deploy (there is
// no Dockerfile on the server).
func prepareRemoteCompose(raw, tag string) string {
	var out []string
	skipBuildBlock := false
	buildIndent := ""
	for _, line := range strings.Split(raw, "\n") {
		// build: .  or  build: ./path — replace in place, keep sibling keys (ports, env, …).
		if m := sameLineBuild.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+"image: "+tag)
			continue
		}
		// build:   (mapping on following lines — skip only its children)
		if m := blockBuild.FindStringSubmatch(line); m != nil {
			buildIndent, skipBuildBlock = m[1], true
			out = append(out, buildIndent+"image: "+tag)
			continue
		}
		if skipBuildBlock {
			if strings.HasPrefix(line, buildIndent+"  ") || strings.HasPrefix(line, buildIndent+"\t") {
				continue
			}
			skipBuildBlock = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// sshSession reuses one SSH connection (ControlMaster) so a password is asked once per deploy.
type sshSession struct {
	port, target, controlPath string
}

func newSSHSession(user, host, port string) *sshSession {
	return &sshSession{
		port:        port,
		target:      user + "@" + host,
		controlPath: filepath.Join(os.TempDir(), fmt.Sprintf("liveprobe-ssh-%s-%s-%s", user, host, port)),
	}
}

func (s *sshSession) multiplex() []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + s.controlPath,
		"-o", "ControlPersist=300",
		"-o", "StrictHostKeyChecking=accept-new",
	}
}

// capture runs a command quietly; on failure the error carries stderr.
func capture(name string, args ...string) error {
	var stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin // let ssh prompt for a password on the terminal
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

func (s *sshSession) open() error { return s.run("true") }

func (s *sshSession) run(remoteCmd ...string) error {
	args := append(append([]string{"-p", s.port}, s.multiplex()...), s.target)
	return capture("ssh", append(args, remoteCmd...)...)
}

func (s *sshSession) copy(local, remote string) error {
	args := append([]string{"-P", s.port}, s.multiplex()...)
	return capture("scp", append(args, local, s.target+":"+remote)...)
}

func (s *sshSession) close() {
	_ = capture("ssh", "-p", s.port, "-o", "ControlPath="+s.controlPath, "-O", "exit", s.target) // master may already be closed
}

// requireDocker ensures plain `docker` works (no sudo).
func requireDocker() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		fail("Cannot access Docker (permission denied).\n\n" +
			"Add your user to the docker group, then log out and back in:\n" +
			"  sudo usermod -aG docker $USER\n" +
			"  newgrp docker\n" +
			"  docker ps\n")
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func main() {
	fmt.Print("=== LiveProbe deploy ===\n\n")
	requireDocker()
	if !fileExists(composeFile) {
		fail("Missing %s", composeFile)
	}

	version, err := resolveVersion(prompt("Image version [latest]: "))
	if err != nil {
		fail("%v", err)
	}
	tag := imageTag(version)
	host := orDefault(prompt("Remote host [your-server.example.com]: "), "your-server.example.com")
	user := orDefault(prompt("SSH username [root]: "), "root")
	port := orDefault(prompt("SSH port [22]: "), "22")
	remoteDir := orDefault(prompt("Remote directory [/var/www/liveprobe]: "), "/var/www/liveprobe")
	if err := validateRemoteDir(remoteDir); err != nil {
		fail("%v", err)
	}

	// Offer to ship a local .env (CORS_ORIGINS, RETENTION_DAYS, HOST, …) only when one exists.
	// Default No: it overwrites the server's .env, which may already hold production config.
	uploadEnv := fileExists(envFile) && yes(prompt("Upload local .env to the server? Overwrites the remote .env [y/N]: "))
	runRemote := yes(prompt("Run `docker compose up -d` on the remote host? [y/N]: "))

	fmt.Println("\nSummary:")
	fmt.Printf("  Image            : %s\n", tag)
	fmt.Printf("  Remote host      : %s@%s:%s\n", user, host, port)
	fmt.Printf("  Remote directory : %s\n", remoteDir)
	fmt.Printf("  Upload .env      : %s\n", map[bool]string{true: "yes (overwrites remote)", false: "no"}[uploadEnv])
	fmt.Printf("  Run remote up    : %s\n", map[bool]string{true: "yes", false: "no"}[runRemote])
	if !yes(prompt("Proceed? [y/N]: ")) {
		fmt.Println("Aborted.")
		return
	}

	tmpDir, err := os.MkdirTemp("", "liveprobe-deploy-")
	if err != nil {
		fail("%v", err)
	}
	var ssh *sshSession
	// fail exits the process, so clean up before reporting any step error.
	cleanup := func() {
		if ssh != nil {
			ssh.close()
		}
		_ = os.RemoveAll(tmpDir)
	}
	step := func(what string, err error) {
		if err != nil {
			cleanup()
			fail("%s failed: %v", what, err)
		}
	}

	imageTar := filepath.Join(tmpDir, "liveprobe-image.tar")
	stagedCompose := filepath.Join(tmpDir, "docker-compose.yml")
	raw, err := os.ReadFile(composeFile)
	step("read compose", err)
	step("stage compose", os.WriteFile(stagedCompose, []byte(prepareRemoteCompose(string(raw), tag)), 0o644))

	fmt.Println("\n==> Building Docker image locally")
	step("docker build", run("docker", "build", "-t", tag, "."))
	fmt.Println("==> Saving Docker image to tarball")
	step("docker save", run("docker", "save", "-o", imageTar, tag))

	ssh = newSSHSession(user, host, port)
	fmt.Println("==> Opening SSH connection")
	step("ssh connect", ssh.open())
	fmt.Println("==> Creating remote directory")
	step("ssh mkdir", ssh.run("mkdir", "-p", remoteDir))
	fmt.Println("==> Uploading image tarball")
	step("scp image", ssh.copy(imageTar, remoteDir+"/"))
	fmt.Println("==> Uploading docker-compose.yml")
	step("scp compose", ssh.copy(stagedCompose, remoteDir+"/docker-compose.yml"))
	if fileExists(collectorConfig) {
		fmt.Println("==> Uploading otel-collector-config.yaml")
		step("scp collector config", ssh.copy(collectorConfig, remoteDir+"/"))
	}
	if uploadEnv {
		fmt.Println("==> Uploading .env")
		step("scp .env", ssh.copy(envFile, remoteDir+"/.env"))
	}

	remoteImageTar := path.Join(remoteDir, "liveprobe-image.tar")
	fmt.Println("==> Loading image on remote host")
	step("remote docker load", ssh.run("docker", "load", "-i", remoteImageTar))
	fmt.Println("==> Removing image tarball on remote host")
	step("remote rm", ssh.run("rm", "-f", remoteImageTar))
	if runRemote {
		fmt.Println("==> Running docker compose up -d on remote host")
		step("remote docker compose up", ssh.run(fmt.Sprintf("cd '%s' && docker compose up -d --no-build", remoteDir)))
	}
	cleanup()

	fmt.Println("\nDeployment complete.")
	fmt.Printf("Remote directory: %s\n", remoteDir)
	fmt.Println("On the server:")
	fmt.Printf("  cd %s && docker compose ps\n", remoteDir)
	fmt.Printf("  cd %s && docker compose logs -f\n", remoteDir)
}
