package python

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bmatcuk/doublestar"
	"github.com/emirpasic/gods/lists/singlylinkedlist"
	"github.com/emirpasic/gods/sets/treeset"
	godsutils "github.com/emirpasic/gods/utils"
	"github.com/google/uuid"

	"github.com/bazelbuild/rules_python/gazelle/pythonconfig"
)

const (
	pyLibraryEntrypointFilename = "__init__.py"
	pyBinaryEntrypointFilename  = "__main__.py"
	pyTestEntrypointFilename    = "__test__.py"
	pyTestEntrypointTargetname  = "__test__"
	conftestFilename            = "conftest.py"
	conftestTargetname          = "conftest"
)

var (
	buildFilenames = []string{"BUILD", "BUILD.bazel"}
)

// GenerateRules extracts build metadata from source files in a directory.
// GenerateRules is called in each directory where an update is requested
// in depth-first post-order.
func (py *Python) GenerateRules(args language.GenerateArgs) language.GenerateResult {
	cfgs := args.Config.Exts[languageName].(pythonconfig.Configs)
	cfg := cfgs[args.Rel]

	if !cfg.ExtensionEnabled() {
		return language.GenerateResult{}
	}

	if !isBazelPackage(args.Dir) {
		if cfg.CoarseGrainedGeneration() {
			// Determine if the current directory is the root of the coarse-grained
			// generation. If not, return without generating anything.
			parent := cfg.Parent()
			if parent != nil && parent.CoarseGrainedGeneration() {
				return language.GenerateResult{}
			}
		} else if cfg.PackageGeneration() && !hasEntrypointFile(args.Dir) {
			return language.GenerateResult{}
		}
	}

	var result language.GenerateResult
	if cfg.PackageGeneration() || cfg.CoarseGrainedGeneration() {
		result = py.generateRulesPackage(cfg, args)
	} else if cfg.ModuleGeneration() {
		result = py.generateRulesModule(cfg, args)
	} else {
		log.Fatalf("unknown generation mode %s", cfg.GenerationMode())
	}
	return result
}

// generateRulesPackage extracts build metadata from source files in a
// directory for the `package` and `project` generation modes.
func (py *Python) generateRulesPackage(cfg *pythonconfig.Config, args language.GenerateArgs) language.GenerateResult {

	pythonProjectRoot := cfg.PythonProjectRoot()

	packageName := filepath.Base(args.Dir)

	pyLibraryFilenames := treeset.NewWith(godsutils.StringComparator)
	pyTestFilenames := treeset.NewWith(godsutils.StringComparator)

	// hasPyBinary controls whether a py_binary target should be generated for
	// this package or not.
	hasPyBinary := false

	// hasPyTestFile and hasPyTestTarget control whether a py_test target should
	// be generated for this package or not.
	hasPyTestFile := false
	hasPyTestTarget := false
	hasConftestFile := false

	for _, f := range args.RegularFiles {
		if cfg.IgnoresFile(filepath.Base(f)) {
			continue
		}
		ext := filepath.Ext(f)
		if !hasPyBinary && f == pyBinaryEntrypointFilename {
			hasPyBinary = true
		} else if !hasPyTestFile && f == pyTestEntrypointFilename {
			hasPyTestFile = true
		} else if f == conftestFilename {
			hasConftestFile = true
		} else if strings.HasSuffix(f, "_test.py") || (strings.HasPrefix(f, "test_") && ext == ".py") {
			pyTestFilenames.Add(f)
		} else if ext == ".py" {
			pyLibraryFilenames.Add(f)
		}
	}

	// If a __test__.py file was not found on disk, search for targets that are
	// named __test__.
	if !hasPyTestFile && args.File != nil {
		for _, rule := range args.File.Rules {
			if rule.Name() == pyTestEntrypointTargetname {
				hasPyTestTarget = true
				break
			}
		}
	}

	// Add files from subdirectories if they meet the criteria.
	for _, d := range args.Subdirs {
		// boundaryPackages represents child Bazel packages that are used as a
		// boundary to stop processing under that tree.
		boundaryPackages := make(map[string]struct{})
		err := filepath.WalkDir(
			filepath.Join(args.Dir, d),
			func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				// Ignore the path if it crosses any boundary package. Walking
				// the tree is still important because subsequent paths can
				// represent files that have not crossed any boundaries.
				for bp := range boundaryPackages {
					if strings.HasPrefix(path, bp) {
						return nil
					}
				}
				if entry.IsDir() {
					// If we are visiting a directory, we determine if we should
					// halt digging the tree based on a few criterias:
					//   1. The directory has a BUILD or BUILD.bazel files. Then
					//       it doesn't matter at all what it has since it's a
					//       separate Bazel package.
					//   2. (only for fine-grained generation) The directory has
					// 		 an __init__.py, __main__.py or __test__.py, meaning
					// 		 a BUILD file will be generated.
					if isBazelPackage(path) {
						boundaryPackages[path] = struct{}{}
						return nil
					}

					if !cfg.CoarseGrainedGeneration() && hasEntrypointFile(path) {
						return fs.SkipDir
					}

					return nil
				}
				if filepath.Ext(path) == ".py" {
					if cfg.CoarseGrainedGeneration() || !isEntrypointFile(path) {
						f, _ := filepath.Rel(args.Dir, path)
						excludedPatterns := cfg.ExcludedPatterns()
						if excludedPatterns != nil {
							it := excludedPatterns.Iterator()
							for it.Next() {
								excludedPattern := it.Value().(string)
								isExcluded, err := doublestar.Match(excludedPattern, f)
								if err != nil {
									return err
								}
								if isExcluded {
									return nil
								}
							}
						}
						baseName := filepath.Base(path)
						if strings.HasSuffix(baseName, "_test.py") || strings.HasPrefix(baseName, "test_") {
							pyTestFilenames.Add(f)
						} else {
							pyLibraryFilenames.Add(f)
						}
					}
				}
				return nil
			},
		)
		if err != nil {
			log.Printf("ERROR: %v\n", err)
			return language.GenerateResult{}
		}
	}

	parser := newPython3Parser(args.Config.RepoRoot, args.Rel, cfg.IgnoresDependency)
	visibility := fmt.Sprintf("//%s:__subpackages__", pythonProjectRoot)

	var result language.GenerateResult
	result.Gen = make([]*rule.Rule, 0)

	collisionErrors := singlylinkedlist.New()

	if !hasPyTestFile && !hasPyTestTarget {
		it := pyTestFilenames.Iterator()
		for it.Next() {
			pyLibraryFilenames.Add(it.Value())
		}
	}

	var pyLibrary *rule.Rule
	if !pyLibraryFilenames.Empty() {
		_, deps, err := parser.parse(pyLibraryFilenames)
		if err != nil {
			log.Fatalf("ERROR: %v\n", err)
		}

		pyLibraryTargetName := cfg.RenderLibraryName(packageName)

		// Check if a target with the same name we are generating already
		// exists, and if it is of a different kind from the one we are
		// generating. If so, we have to throw an error since Gazelle won't
		// generate it correctly.
		if args.File != nil {
			for _, t := range args.File.Rules {
				if t.Name() == pyLibraryTargetName && t.Kind() != pyLibraryKind {
					fqTarget := label.New("", args.Rel, pyLibraryTargetName)
					err := fmt.Errorf("failed to generate target %q of kind %q: "+
						"a target of kind %q with the same name already exists. "+
						"Use the '# gazelle:%s' directive to change the naming convention.",
						fqTarget.String(), pyLibraryKind, t.Kind(), pythonconfig.LibraryNamingConvention)
					collisionErrors.Add(err)
				}
			}
		}

		pyLibrary = newTargetBuilder(pyLibraryKind, pyLibraryTargetName, pythonProjectRoot, args.Rel).
			setUUID(uuid.Must(uuid.NewUUID()).String()).
			addVisibility(visibility).
			addSrcs(pyLibraryFilenames).
			addModuleDependencies(deps).
			generateImportsAttribute().
			build()

		result.Gen = append(result.Gen, pyLibrary)
		result.Imports = append(result.Imports, pyLibrary.PrivateAttr(config.GazelleImportsKey))
	}

	if hasPyBinary {
		_, deps, err := parser.parseSingle(pyBinaryEntrypointFilename)
		if err != nil {
			log.Fatalf("ERROR: %v\n", err)
		}

		pyBinaryTargetName := cfg.RenderBinaryName(packageName)

		// Check if a target with the same name we are generating already
		// exists, and if it is of a different kind from the one we are
		// generating. If so, we have to throw an error since Gazelle won't
		// generate it correctly.
		if args.File != nil {
			for _, t := range args.File.Rules {
				if t.Name() == pyBinaryTargetName && t.Kind() != pyBinaryKind {
					fqTarget := label.New("", args.Rel, pyBinaryTargetName)
					err := fmt.Errorf("failed to generate target %q of kind %q: "+
						"a target of kind %q with the same name already exists. "+
						"Use the '# gazelle:%s' directive to change the naming convention.",
						fqTarget.String(), pyBinaryKind, t.Kind(), pythonconfig.BinaryNamingConvention)
					collisionErrors.Add(err)
				}
			}
		}

		pyBinaryTarget := newTargetBuilder(pyBinaryKind, pyBinaryTargetName, pythonProjectRoot, args.Rel).
			setMain(pyBinaryEntrypointFilename).
			addVisibility(visibility).
			addSrc(pyBinaryEntrypointFilename).
			addModuleDependencies(deps).
			generateImportsAttribute()

		if pyLibrary != nil {
			pyBinaryTarget.addModuleDependency(module{Name: pyLibrary.PrivateAttr(uuidKey).(string)})
		}

		pyBinary := pyBinaryTarget.build()

		result.Gen = append(result.Gen, pyBinary)
		result.Imports = append(result.Imports, pyBinary.PrivateAttr(config.GazelleImportsKey))
	}

	var conftest *rule.Rule
	if hasConftestFile {
		_, deps, err := parser.parseSingle(conftestFilename)
		if err != nil {
			log.Fatalf("ERROR: %v\n", err)
		}

		// Check if a target with the same name we are generating already
		// exists, and if it is of a different kind from the one we are
		// generating. If so, we have to throw an error since Gazelle won't
		// generate it correctly.
		if args.File != nil {
			for _, t := range args.File.Rules {
				if t.Name() == conftestTargetname && t.Kind() != pyLibraryKind {
					fqTarget := label.New("", args.Rel, conftestTargetname)
					err := fmt.Errorf("failed to generate target %q of kind %q: "+
						"a target of kind %q with the same name already exists.",
						fqTarget.String(), pyLibraryKind, t.Kind())
					collisionErrors.Add(err)
				}
			}
		}

		conftestTarget := newTargetBuilder(pyLibraryKind, conftestTargetname, pythonProjectRoot, args.Rel).
			setUUID(uuid.Must(uuid.NewUUID()).String()).
			addSrc(conftestFilename).
			addModuleDependencies(deps).
			addVisibility(visibility).
			setTestonly().
			generateImportsAttribute()

		conftest = conftestTarget.build()

		result.Gen = append(result.Gen, conftest)
		result.Imports = append(result.Imports, conftest.PrivateAttr(config.GazelleImportsKey))
	}

	if hasPyTestFile || hasPyTestTarget {
		if hasPyTestFile {
			// Only add the pyTestEntrypointFilename to the pyTestFilenames if
			// the file exists on disk.
			pyTestFilenames.Add(pyTestEntrypointFilename)
		}
		_, deps, err := parser.parse(pyTestFilenames)
		if err != nil {
			log.Fatalf("ERROR: %v\n", err)
		}

		pyTestTargetName := cfg.RenderTestName(packageName)

		// Check if a target with the same name we are generating already
		// exists, and if it is of a different kind from the one we are
		// generating. If so, we have to throw an error since Gazelle won't
		// generate it correctly.
		if args.File != nil {
			for _, t := range args.File.Rules {
				if t.Name() == pyTestTargetName && t.Kind() != pyTestKind {
					fqTarget := label.New("", args.Rel, pyTestTargetName)
					err := fmt.Errorf("failed to generate target %q of kind %q: "+
						"a target of kind %q with the same name already exists. "+
						"Use the '# gazelle:%s' directive to change the naming convention.",
						fqTarget.String(), pyTestKind, t.Kind(), pythonconfig.TestNamingConvention)
					collisionErrors.Add(err)
				}
			}
		}

		pyTestTarget := newTargetBuilder(pyTestKind, pyTestTargetName, pythonProjectRoot, args.Rel).
			addSrcs(pyTestFilenames).
			addModuleDependencies(deps).
			generateImportsAttribute()

		if hasPyTestTarget {
			entrypointTarget := fmt.Sprintf(":%s", pyTestEntrypointTargetname)
			main := fmt.Sprintf(":%s", pyTestEntrypointFilename)
			pyTestTarget.
				addSrc(entrypointTarget).
				addResolvedDependency(entrypointTarget).
				setMain(main)
		} else {
			pyTestTarget.setMain(pyTestEntrypointFilename)
		}

		if pyLibrary != nil {
			pyTestTarget.addModuleDependency(module{Name: pyLibrary.PrivateAttr(uuidKey).(string)})
		}

		if conftest != nil {
			pyTestTarget.addModuleDependency(module{Name: conftest.PrivateAttr(uuidKey).(string)})
		}

		pyTest := pyTestTarget.build()

		result.Gen = append(result.Gen, pyTest)
		result.Imports = append(result.Imports, pyTest.PrivateAttr(config.GazelleImportsKey))
	}

	if !collisionErrors.Empty() {
		it := collisionErrors.Iterator()
		for it.Next() {
			log.Printf("ERROR: %v\n", it.Value())
		}
		os.Exit(1)
	}
	return result
}

// generateRulesModule extracts build metadata from source files in a
// directory for the `module` generation modes.
//
// Every module is either:
// 1.                  -> py_library
// 2.          + main  -> py_binary
// 3. test name        -> py_library
// 3. test name + main -> py_test
//
// NOTE: Currently all module targets use the module naming convention, not the
// package, library, or test naming conventions. By default, this means all
// modules will be named exactly the name of their module.
//
// Additionally, if the filename matches the package name and a __init__.py is
// present, that will not work because we will have a conflict: the __init__.py
// will produce a target of the same name.
func (py *Python) generateRulesModule(cfg *pythonconfig.Config, args language.GenerateArgs) language.GenerateResult {

	pythonProjectRoot := cfg.PythonProjectRoot()

	packageName := filepath.Base(args.Dir)

	pyFilenames := treeset.NewWith(godsutils.StringComparator)

	// Make a list of all python files, we'll check what they are later.
	for _, f := range args.RegularFiles {
		if cfg.IgnoresFile(filepath.Base(f)) {
			continue
		}

		if filepath.Ext(f) == ".py" {
			pyFilenames.Add(f)
		}
	}

	parser := newPython3Parser(
		args.Config.RepoRoot, args.Rel, cfg.IgnoresDependency)

	var result language.GenerateResult
	result.Gen = make([]*rule.Rule, 0)

	collisionErrors := singlylinkedlist.New()

	It := pyFilenames.Iterator()

	for It.Next() {
		// Get the dependencies of this python module
		fileName := It.Value().(string)
		moduleName := getModuleName(fileName)

		hasMain, deps, err := parser.parseSingle(It.Value().(string))

		if err != nil {
			log.Fatalf("ERROR: %v\n", err)
		}

		uiudkey := uuid.Must(uuid.NewUUID()).String()

		// Only create a test if it matches the test naming scheme AND it has
		// a __main__ dunder.
		if isPyTest(fileName) && hasMain {

			// Tests use the module name.
			pyTestTargetName := cfg.RenderModuleName(moduleName)

			// Check for a collision
			err := checkCollision(args, pyTestTargetName, pyTestKind, pythonconfig.ModuleNamingConvention)
			if err != nil {
				collisionErrors.Add(err)
			}

			pyTestTarget := newTargetBuilder(pyTestKind, pyTestTargetName, pythonProjectRoot, args.Rel).
				addSrc(fileName).
				addModuleDependencies(deps).
				generateImportsAttribute().
				setMain(fileName)

			pyTest := pyTestTarget.build()

			result.Gen = append(result.Gen, pyTest)
			result.Imports = append(result.Imports, pyTest.PrivateAttr(config.GazelleImportsKey))

			// A py_test makes no other targets.
			continue
		}

		// We create a binary iff there is a main attribute, otherwise create a
		// library.
		if hasMain {

			pyBinaryTargetName := cfg.RenderModuleName(moduleName)

			// Check for collisions
			err := checkCollision(args, pyBinaryTargetName, pyBinaryKind, pythonconfig.ModuleNamingConvention)
			if err != nil {
				collisionErrors.Add(err)
			}

			pyBinaryTarget := newTargetBuilder(pyBinaryKind, pyBinaryTargetName, pythonProjectRoot, args.Rel).
				setMain(fileName).
				addSrc(fileName).
				addModuleDependencies(deps).
				generateImportsAttribute()

			pyBinary := pyBinaryTarget.build()

			result.Gen = append(result.Gen, pyBinary)
			result.Imports = append(result.Imports, pyBinary.PrivateAttr(config.GazelleImportsKey))
		} else {
			// If the filename is __init__.py, use the directory name. Otherwise use
			// the filename.
			var pyLibraryTargetName string
			if fileName == pyLibraryEntrypointFilename {
				pyLibraryTargetName = cfg.RenderLibraryName(packageName)
			} else {
				pyLibraryTargetName = cfg.RenderModuleName(moduleName)
			}

			// Check for a collision
			err := checkCollision(args, pyLibraryTargetName, pyLibraryKind, pythonconfig.ModuleNamingConvention)
			if err != nil {
				collisionErrors.Add(err)
			}

			pyLibraryBuilder := newTargetBuilder(pyLibraryKind, pyLibraryTargetName, pythonProjectRoot, args.Rel).
				setUUID(uiudkey).
				addSrc(fileName).
				addModuleDependencies(deps).
				generateImportsAttribute()

			pyLibrary := pyLibraryBuilder.build()

			result.Gen = append(result.Gen, pyLibrary)
			result.Imports = append(result.Imports, pyLibrary.PrivateAttr(config.GazelleImportsKey))
		}
	}

	if !collisionErrors.Empty() {
		it := collisionErrors.Iterator()
		for it.Next() {
			log.Printf("ERROR: %v\n", it.Value())
		}
		os.Exit(1)
	}

	return result
}

// Checks if a target with the same name we are generating already exists, and
// if it is of a different kind from the one we are generating. If so, this will
// return an error. Note that this returns only the first such error.
func checkCollision(args language.GenerateArgs, targetName, targetKind, namingConvention string) error {

	if args.File != nil {
		for _, t := range args.File.Rules {
			if t.Name() == targetName && t.Kind() != targetKind {
				fqTarget := label.New("", args.Rel, targetName)
				err := fmt.Errorf("failed to generate target %q of kind %q: "+
					"a target of kind %q with the same name already exists. "+
					"Use the '# gazelle:%s' directive to change the naming convention.",
					fqTarget.String(), targetKind, t.Kind(), namingConvention)
				return err
			}
		}
	}
	return nil
}

// isPyTest determines if a file is a test based on the suffix. Any file with
// the suffix `_test.py` is considered a test.
// TODO(corypaik): Consider parameterizing this s.t. it supports other methods
// of path-based matching (e.g., test/* or test_*.py).
func isPyTest(f string) bool {
	ext := filepath.Ext(f)
	return strings.HasSuffix(f, "_test.py") && ext == ".py"
}

// getModuleName determines a module's name based on the file name.
func getModuleName(fileName string) string {
	if pos := strings.LastIndexByte(fileName, '.'); pos != -1 {
		return fileName[:pos]
	}
	return fileName
}

// isBazelPackage determines if the directory is a Bazel package by probing for
// the existence of a known BUILD file name.
func isBazelPackage(dir string) bool {
	for _, buildFilename := range buildFilenames {
		path := filepath.Join(dir, buildFilename)
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// hasEntrypointFile determines if the directory has any of the established
// entrypoint filenames.
func hasEntrypointFile(dir string) bool {
	for _, entrypointFilename := range []string{
		pyLibraryEntrypointFilename,
		pyBinaryEntrypointFilename,
		pyTestEntrypointFilename,
	} {
		path := filepath.Join(dir, entrypointFilename)
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// isEntrypointFile returns whether the given path is an entrypoint file. The
// given path can be absolute or relative.
func isEntrypointFile(path string) bool {
	basePath := filepath.Base(path)
	switch basePath {
	case pyLibraryEntrypointFilename,
		pyBinaryEntrypointFilename,
		pyTestEntrypointFilename:
		return true
	default:
		return false
	}
}
