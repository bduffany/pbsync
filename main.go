package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bazelbuild/buildtools/build"
)

const (
	goProtoLibrary = "go_proto_library"
	tsProtoLibrary = "ts_proto_library"

	bazelBinKey = "bazel-bin"
)

var (
	githubRepoRe = regexp.MustCompile(`^github.com/(.+?)/(.+?)/`)
)

func getBazelBinDir(workspaceRoot string) (string, error) {
	// The `bazel info` command is unfortunately super slow (lame).
	// So we cache it.
	cached, err := cacheGet(cacheKey(bazelBinKey, workspaceRoot))
	if err != nil {
		return "", err
	}
	if cached != "" {
		return cached, nil
	}
	value, err := computeBazelBinDir(workspaceRoot)
	if err != nil {
		return "", err
	}
	if err := cacheSet(cacheKey(bazelBinKey, workspaceRoot), value); err != nil {
		return "", err
	}
	return value, nil
}

func cacheKey(keys ...string) string {
	var b []byte
	for _, k := range keys {
		bk := sha256.Sum256([]byte(k))
		b = append(b, bk[:]...)
	}
	return fmt.Sprintf("%x", b)
}

func cachePath(key string) (string, error) {
	sha := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheRoot, sha), nil
}

func cacheGet(key string) (value string, err error) {
	path, err := cachePath(key)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
	}
	return string(b), nil
}

func cacheSet(key, value string) error {
	path, err := cachePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0644)
}

func computeBazelBinDir(workspaceRoot string) (string, error) {
	cmd := exec.Command("bazel", "info", "--show_make_env")
	cmd.Dir = workspaceRoot
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "bazel-bin:" {
			continue
		}
		return fields[1], nil
	}
	return "", fmt.Errorf("missing 'bazel-bin' entry in `bazel info --show_make_env`")
}

type languageProtoRule struct {
	kind, name, protoRuleName, importPath string
}

func (r *languageProtoRule) getSrcAndDest(workspaceRoot, bazelBin, protoPath string) (string, string, error) {
	protoRelpath := strings.TrimPrefix(protoPath, workspaceRoot)

	switch r.kind {

	case goProtoLibrary:
		wsRelpath := githubRepoRe.ReplaceAllLiteralString(r.importPath, "")
		if wsRelpath == r.importPath {
			return "", "", fmt.Errorf("could not figure out workspace relative path for import %q", r.importPath)
		}
		protoBase := filepath.Base(protoPath)
		genBase := strings.TrimSuffix(protoBase, ".proto") + ".pb.go"
		src := filepath.Join(bazelBin, filepath.Dir(protoRelpath), r.name+"_", r.importPath, genBase)
		dest := filepath.Join(workspaceRoot, wsRelpath, genBase)
		return src, dest, nil

	case tsProtoLibrary:
		src := filepath.Join(bazelBin, filepath.Dir(protoRelpath), r.name+".d.ts")
		dest := filepath.Join(workspaceRoot, filepath.Dir(protoRelpath), r.name+".d.ts")
		return src, dest, nil

	}
	return "", "", fmt.Errorf("unknown proto rule kind %q", r.kind)
}

type parsedBuildFile struct {
	protoFileToRule           map[string]string
	protoRuleToLangProtoRules map[string][]languageProtoRule
}

func (b *parsedBuildFile) getLangProtoRulesForProto(protoFile string) ([]languageProtoRule, bool) {
	basename := filepath.Base(protoFile)
	protoRule, ok := b.protoFileToRule[basename]
	if !ok {
		return nil, false
	}
	langRules, ok := b.protoRuleToLangProtoRules[protoRule]
	if !ok {
		return nil, false
	}
	return langRules, true
}

func parseBuildFile(buildFilePath string) (*parsedBuildFile, error) {
	buildFileContents, err := ioutil.ReadFile(buildFilePath)
	if err != nil {
		return nil, fmt.Errorf("could not read BUILD file %q: %v", buildFilePath, err)
	}
	buildFile, err := build.ParseBuild(filepath.Base(buildFilePath), buildFileContents)
	if err != nil {
		return nil, fmt.Errorf("could not parse BUILD file %q: %v", buildFilePath, err)
	}

	protoFileToRule := make(map[string]string)

	protoRules := buildFile.Rules("proto_library")
	for _, r := range protoRules {
		srcs := r.AttrStrings("srcs")
		if srcs == nil {
			return nil, fmt.Errorf("%s: proto rule %q does not have have srcs", buildFilePath, r.Name())
		}
		for _, src := range srcs {
			src = strings.TrimPrefix(src, ":")
			if protoFileToRule[src] != "" {
				return nil, fmt.Errorf("%s: src file %q appears in multiple proto rules", buildFilePath, src)
			}
			protoFileToRule[src] = r.Name()
		}
	}

	protoRuleToLangProtoRules := make(map[string][]languageProtoRule)

	goProtoRules := buildFile.Rules("")
	for _, r := range goProtoRules {
		if r.Kind() != goProtoLibrary && r.Kind() != tsProtoLibrary {
			continue
		}

		protoRule := r.AttrString("proto")
		if protoRule == "" {
			return nil, fmt.Errorf("%s: go proto rule %q missing proto attribute", buildFilePath, r.Name())
		}
		if !strings.HasPrefix(protoRule, ":") {
			// fmt.Printf("%s: go proto rule %q has unsupported proto reference: %s\n", buildFilePath, r.Name(), protoRule)
			continue
		}

		importPath := ""
		if r.Kind() == goProtoLibrary {
			importPath = r.AttrString("importpath")
			if importPath == "" {
				return nil, fmt.Errorf("%s: go proto rule %q missing importpath attribute", buildFilePath, r.Name())
			}
		}

		protoRuleName := protoRule[1:]
		langProtoRule := languageProtoRule{
			kind:          r.Kind(),
			name:          r.Name(),
			protoRuleName: protoRule[1:],
			importPath:    importPath,
		}
		protoRuleToLangProtoRules[protoRuleName] = append(protoRuleToLangProtoRules[protoRuleName], langProtoRule)
	}

	return &parsedBuildFile{
		protoFileToRule:           protoFileToRule,
		protoRuleToLangProtoRules: protoRuleToLangProtoRules,
	}, nil
}

type result struct {
	created  int
	upToDate int
}

func syncProto(workspaceRoot string, protoFile string, buildFile *parsedBuildFile, result *result) error {
	rules, ok := buildFile.getLangProtoRulesForProto(protoFile)
	if !ok {
		fmt.Printf("could not figure out proto rule for %q\n", protoFile)
		return nil
	}

	bazelBin, err := getBazelBinDir(workspaceRoot)
	if err != nil {
		return fmt.Errorf("failed to determine bazel bin dir: %s", err)
	}

	for _, rule := range rules {
		src, dest, err := rule.getSrcAndDest(workspaceRoot, bazelBin, protoFile)
		if err != nil {
			return err
		}

		// Read the generated source
		sb, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				// Skip; the generated source is not available.
				continue
			}
			return err
		}
		sourceContent := string(sb)
		if sourceContent == "" {
			return fmt.Errorf("file is unexpectedly empty: %s", protoFile)
		}

		// Read the existing target file
		db, err := os.ReadFile(dest)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		destContent := string(db)

		if sourceContent == destContent {
			result.upToDate++
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, sb, 0644); err != nil {
			return err
		}

		result.created++
	}
	return nil
}

func copyGeneratedProtos(workspaceRoot string) (*result, error) {
	_, err := os.Stat(filepath.Join(workspaceRoot, "WORKSPACE"))
	if err != nil {
		return nil, fmt.Errorf("%q does not appear to be a Bazel workspace (no WORKSPACE file): %s", workspaceRoot, err)
	}

	// Get proto source paths (use the git index for speed)
	var protos []string
	lsFiles := exec.Command("git", "ls-files", "--exclude-standard")
	lsFiles.Dir = workspaceRoot
	lsFiles.Stderr = os.Stderr
	buf := &bytes.Buffer{}
	lsFiles.Stdout = buf
	if err := lsFiles.Run(); err != nil {
		return nil, fmt.Errorf("failed to list proto sources: git ls-files failed")
	}
	for _, path := range strings.Split(buf.String(), "\n") {
		if strings.HasSuffix(path, ".proto") {
			protos = append(protos, filepath.Join(workspaceRoot, path))
		}
	}

	result := &result{}

	buildFiles := make(map[string]*parsedBuildFile)

	for _, proto := range protos {
		// For now only support build files named "BUILD".
		buildFilePath := filepath.Join(filepath.Dir(proto), "BUILD")
		buildFile := buildFiles[buildFilePath]
		if buildFile == nil {
			// Ignore protos that are not in bazel packages.
			if _, err := os.Stat(buildFilePath); err != nil {
				continue
			}
			buildFile, err = parseBuildFile(buildFilePath)
			if err != nil {
				return nil, fmt.Errorf("could not parse BUILD file %q: %v", buildFilePath, err)
			}
			buildFiles[buildFilePath] = buildFile
		}

		if err := syncProto(workspaceRoot, proto, buildFile, result); err != nil {
			return nil, err
		}

	}
	return result, nil
}

func fatalf(msg string, args ...any) {
	fmt.Printf("pbsync: "+msg+"\n", args...)
	os.Exit(1)
}

func main() {
	start := time.Now()

	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			fatalf("failed to determine working dir: %s", err)
		}
		dirs = append(dirs, cwd)
	}

	total := &result{}

	for _, dir := range dirs {
		result, err := copyGeneratedProtos(dir)
		if err != nil {
			fatalf("failed to sync protos for workspace %s: %s", dir, err)
		}
		total.created += result.created
		total.upToDate += result.upToDate
	}

	fmt.Printf("pbsync: wrote %d protos in %s (%d up to date)\n", total.created, time.Since(start), total.upToDate)
}
