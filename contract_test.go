package kvstore

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The contract names tests. This checks they still exist.
//
// A red proof is a historical record: the harness stores that a case was once
// observed failing, with the date and the contract hash. What it cannot store
// is whether the test that produced it is still in the tree. Delete the file
// and the proof stays behind, still counted as proven, and every leg stays
// green - which was demonstrated rather than assumed: journalleddir_test.go
// was moved out of the tree and `general-verify --require-red-proof` reported
// 44/50 proven and GREEN with the tests it was vouching for gone.
//
// That is the one direction the harness genuinely cannot check itself, because
// it is a question about this repository's own tree rather than about the
// gate. So it is checked here, where deleting the test file and deleting the
// case that names it have to happen in the same commit or the suite goes red.
//
// Only cases carrying an explicit test_name are checked. The rest are matched
// by the harness's own id-to-name heuristic, and re-deriving that here would
// be a second implementation of it that could drift from the first - the exact
// mistake the mutation sweep's header warns about.
func TestEveryTestTheContractNamesStillExists(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("verify", "kvstore.task.json"))
	if err != nil {
		t.Fatalf("reading the task contract: %v", err)
	}

	var contract struct {
		Units []struct {
			ID    string `json:"id"`
			Cases []struct {
				ID       string `json:"id"`
				TestName string `json:"test_name"`
			} `json:"cases"`
		} `json:"units"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("parsing the task contract: %v", err)
	}

	present := testFunctionsInTree(t)

	named := 0
	for _, unit := range contract.Units {
		for _, c := range unit.Cases {
			if c.TestName == "" {
				continue
			}
			named++
			if !present[c.TestName] {
				t.Errorf("%s/%s names %s, and no such test function is in the tree - if the test was deleted or renamed, the case goes with it in the same commit; a case whose test is gone still counts as proven and that green means nothing",
					unit.ID, c.ID, c.TestName)
			}
		}
	}

	// A contract that stopped naming tests would make this test vacuous while
	// leaving it green, which is the shape of failure the whole repository is
	// about. The floor is deliberately below the current count so adding and
	// removing a case is not a two-file edit, and far enough above zero that
	// the checking cannot quietly stop.
	const floor = 25
	if named < floor {
		t.Errorf("only %d case(s) name a test; the floor is %d. Either the contract has stopped pinning its mappings, or this test has stopped checking anything", named, floor)
	}
	if !t.Failed() {
		t.Logf("%d case(s) name a test, all present", named)
	}
}

// testFunctionsInTree collects every `func TestX(` in every _test.go file
// under the module, including the crashtest package, because the contract
// names cases in both.
func testFunctionsInTree(t *testing.T) map[string]bool {
	t.Helper()

	decl := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
	found := make(map[string]bool)

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing generated or vendored should be able to satisfy a
			// contract mapping.
			if name := d.Name(); name == ".git" || name == "crash-failures" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range decl.FindAllStringSubmatch(string(body), -1) {
			found[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree for test functions: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no test functions found anywhere in the tree - the walk is broken, not the contract")
	}
	return found
}
