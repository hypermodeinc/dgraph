package testaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/stretchr/testify/require"
)

func VerifyLogs(t *testing.T, path string, cmds []string) {
	abs, err := filepath.Abs(path)
	require.Nil(t, err)
	f, err := os.Open(abs)
	require.Nil(t, err)

	type log struct {
		Msg string `json:"endpoint"`
	}
	logMap := make(map[string]bool)

	fileScanner := bufio.NewScanner(f)
	for fileScanner.Scan() {
		bytes := fileScanner.Bytes()
		l := new(log)
		_ = json.Unmarshal(bytes, l)
		logMap[l.Msg] = true
	}
	for _, m := range cmds {
		if !logMap[m] {
			t.Fatalf("audit logs not present for command %s", m)
		}
	}
}

// run manually to generate the encrypted audit log used in TestDecrypt
func TestGenerateAuditForTestDecrypt(t *testing.T) {
	// to generate audit logs, uncomment and run ./t --test=TestGenerateAuditForTestDecrypt
	t.Skip()
	zeroCmd := map[string][]string{
		"/removeNode": {`--location`, "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/removeNode?id=3&group=1", testutil.SockAddrZeroHttp)},
		"/assign": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/assign?what=uids&num=100", testutil.SockAddrZeroHttp)},
		"/moveTablet": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/moveTablet?tablet=name&group=2", testutil.SockAddrZeroHttp)}}

	for _, c := range zeroCmd {
		cmd := exec.Command("curl", c...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Println(string(out))
			t.Fatal(err)
		}
	}
}
