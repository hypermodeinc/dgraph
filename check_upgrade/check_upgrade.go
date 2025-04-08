/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package checkupgrade

import (
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	CheckUpgrade x.SubCommand
)

const (
	alphaHttp            = "http_port"
	dgUser               = "dgUser"
	password             = "password"
	namespace            = "namespace"
	aclSecretKeyFilePath = "aclSecretKeyFilePath"
	jwtAlg               = "jwt-alg"
	deleteDup            = "delete-duplicates"
	guardianGroup        = "guardians"
)

type commandInput struct {
	alphaHttp            string
	dgUser               string
	password             string
	namespace            uint64
	aclSecretKeyFilePath string
	jwtAlg               string
	dupDelete            bool
}

type aclNode struct {
	UID             string      `json:"uid"`
	DgraphXID       string      `json:"dgraph.xid"`
	DgraphType      []string    `json:"dgraph.type"`
	DgraphUserGroup []UserGroup `json:"dgraph.user.group"`
}

type UserGroup struct {
	UID       string `json:"uid"`
	DgraphXid string `json:"dgraph.xid"`
}

type Nodes struct {
	Nodes []aclNode `json:"nodes"`
}
type Response struct {
	Data Nodes `json:"data"`
}

func parseJWTKey(alg jwt.SigningMethod, key x.Sensitive) (interface{}, interface{}, error) {
	switch {
	case strings.HasPrefix(alg.Alg(), "HS"):
		return key, key, nil

	case strings.HasPrefix(alg.Alg(), "ES"):
		pk, err := jwt.ParseECPrivateKeyFromPEM(key)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing ACL key as ECDSA private key: %w", err)
		}
		return pk, &pk.PublicKey, nil

	case strings.HasPrefix(alg.Alg(), "RS") || strings.HasPrefix(alg.Alg(), "PS"):
		pk, err := jwt.ParseRSAPrivateKeyFromPEM(key)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing ACL key as RSA private key: %w", err)
		}
		return pk, &pk.PublicKey, nil

	case alg.Alg() == "EdDSA":
		pk, err := jwt.ParseEdPrivateKeyFromPEM(key)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing ACL key as EdDSA private key: %w", err)
		}
		return pk.(crypto.Signer), pk.(ed25519.PrivateKey).Public(), nil

	default:
		return nil, nil, fmt.Errorf("unsupported signing algorithm: %v", alg.Alg())
	}
}

func getAccessJwt(userId string, group string, namespace uint64, aclSecretFile string,
	algStr string) (string, error) {
	aclKey, err := os.ReadFile(aclSecretFile)
	if err != nil {
		return "", fmt.Errorf("error reading ACL secret key from file: %s: %s", aclSecretFile, err)
	}

	var aclAlg jwt.SigningMethod
	var privKey interface{}
	if aclKey != nil {
		aclAlg = jwt.GetSigningMethod(algStr)
		if aclAlg == nil {
			return "", fmt.Errorf("unsupported jwt signing algorithm for acl: %v", algStr)
		}
		privKey, _, err = parseJWTKey(aclAlg, aclKey)
		if err != nil {
			return "", err
		}
	}

	token := jwt.NewWithClaims(aclAlg, jwt.MapClaims{
		"userid":    userId,
		"groups":    []string{group},
		"namespace": namespace,
		"exp":       time.Now().Add(time.Hour).Unix(),
	})

	jwtString, err := token.SignedString(x.MaybeKeyToBytes(privKey))
	if err != nil {
		return "", fmt.Errorf("unable to encode jwt to string: %w", err)
	}
	return jwtString, nil
}

func setupClient(alphaHttp string) (*dgraphapi.HTTPClient, error) {
	httpClient, err := dgraphapi.GetHttpClient(alphaHttp, "")
	if err != nil {
		return nil, fmt.Errorf("while getting HTTP client: %w", err)
	}
	return httpClient, nil
}

func contains(slice []string, value string) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func findDuplicateNodes(aclNodes []aclNode) [3]map[string][]string {
	du := make(map[string][]string)
	dg := make(map[string][]string)
	dug := make(map[string][]string)

	for i, node1 := range aclNodes {
		for j := i + 1; j < len(aclNodes); j++ {
			node2 := aclNodes[j]
			if node1.DgraphXID == node2.DgraphXID {
				if node1.DgraphType[0] == "dgraph.type.User" && node1.DgraphType[0] == node2.DgraphType[0] {
					filterAndRecordDuplicates(du, node1, node2)
				} else if node1.DgraphType[0] == "dgraph.type.Group" && node1.DgraphType[0] == node2.DgraphType[0] {
					filterAndRecordDuplicates(dg, node1, node2)
				} else {
					filterAndRecordDuplicates(dug, node1, node2)
				}
			}
		}
	}

	return [3]map[string][]string{
		du, dg, dug,
	}
}

func filterAndRecordDuplicates(du map[string][]string, node1 aclNode, node2 aclNode) {
	if _, exists := du[node1.DgraphXID]; !exists {
		du[node1.DgraphXID] = []string{}
	}
	if !contains(du[node1.DgraphXID], node1.UID) {
		du[node1.DgraphXID] = append(du[node1.DgraphXID], node1.UID)
	}
	if !contains(du[node1.DgraphXID], node2.UID) {
		du[node1.DgraphXID] = append(du[node1.DgraphXID], node2.UID)
	}
}

func queryDuplicateNodes(hc *dgraphapi.HTTPClient) ([3]map[string][]string, error) {
	query := `{ 
		nodes(func: has(dgraph.xid)) {
			       uid
			       dgraph.xid
                   dgraph.type
		        }
	}`

	resp, err := hc.PostDqlQuery(query)
	if err != nil {
		return [3]map[string][]string{}, fmt.Errorf("while querying dgraph for duplicate nodes: %w", err)
	}

	var result Response
	if err := json.Unmarshal(resp, &result); err != nil {
		return [3]map[string][]string{}, fmt.Errorf("while unmarshalling response: %v: %w", string(resp), err)

	}
	return findDuplicateNodes(result.Data.Nodes), nil
}

func printAndDeleteDuplicates(hc *dgraphapi.HTTPClient, entityType string, ns uint64, nodesmap map[string][]string,
	dupDelete bool) error {
	if len(nodesmap) == 0 {
		return nil
	}

	fmt.Printf("Found duplicate %ss in namespace: #%v\n", entityType, ns)
	for key, node := range nodesmap {
		fmt.Printf("dgraph.xid %v , Uids: %v\n", key, node)
	}

	if dupDelete {
		switch entityType {
		case "user":
			return deleteDuplicatesUser(hc, nodesmap)

		case "group":
			return deleteDuplicatesGroup(hc, nodesmap)
		default:
			return deleteDuplicatesUserGroup(hc, nodesmap)

		}
	}
	return nil
}
func deleteUids(hc *dgraphapi.HTTPClient, uids []string, skipUid int, node string) error {
	query := `{
		delete {
			<%v> * * .
		}
	}`

	for i, uid := range uids {
		if i == skipUid {
			continue
		}

		fmt.Printf("deleting following uid [%v] of duplicate node:%v\n", uid, node)
		_, err := hc.Mutate(fmt.Sprintf(query, uid), true)
		if err != nil {
			return err
		}
	}

	return nil
}

// uniqueStringsExcluding extracts unique strings from nodeCollection excluding those in the exclude slice
func uniqueStringsExcluding(nodeCollection [][]string, exclude []string) []string {
	excludeMap := make(map[string]struct{})
	for _, e := range exclude {
		excludeMap[e] = struct{}{}
	}

	uniqueMap := make(map[string]struct{})
	for _, nodes := range nodeCollection {
		for _, node := range nodes {
			if _, inExclude := excludeMap[node]; !inExclude {
				uniqueMap[node] = struct{}{}
			}
		}
	}

	uniqueSlice := make([]string, 0, len(uniqueMap))
	for node := range uniqueMap {
		uniqueSlice = append(uniqueSlice, node)
	}

	return uniqueSlice
}

func queryUserGroup(hc *dgraphapi.HTTPClient, uid string) (aclNode, error) {
	query := fmt.Sprintf(`{ 
		nodes(func: eq("dgraph.xid", "%v")) {
			uid
			dgraph.xid
		}
	}`, uid)
	resp, err := hc.PostDqlQuery(query)
	if err != nil {
		return aclNode{}, err
	}
	var result Response
	if err := json.Unmarshal(resp, &result); err != nil {
		return aclNode{}, fmt.Errorf("while unmarshalling response: %v: %w", string(resp), err)
	}

	if len(result.Data.Nodes) > 1 {
		return aclNode{}, nil
	}

	return result.Data.Nodes[0], nil
}

func addUsersToGroup(hc *dgraphapi.HTTPClient, users []string, groupUid string) error {
	rdf := ``
	for _, user := range users {
		fmt.Printf("adding user %v to group %v\n", user, groupUid)
		node, err := queryUserGroup(hc, user)
		if err != nil {
			return err
		}
		if node.UID != "" {
			rdf += fmt.Sprintf("<%v> <dgraph.user.group> <%v> .\n", groupUid, node.UID)

		}
	}

	_, err := hc.Mutate(rdf, true)
	if err != nil {
		return err
	}
	return nil
}

func deleteDuplicatesGroup(hc *dgraphapi.HTTPClient, duplicates map[string][]string) error {
	query := `{
		nodes(func: uid(%v)) {
			uid
			dgraph.xid  
			dgraph.type
			~dgraph.user.group{
				dgraph.xid  
			}
		}
	}`

	for group, uids := range duplicates {
		var nodeCollection [][]string

		for _, uid := range uids {
			resp, err := hc.PostDqlQuery(fmt.Sprintf(query, uid))
			if err != nil {
				return err
			}
			var result Response
			if err := json.Unmarshal(resp, &result); err != nil {
				log.Fatalf("while unmarshalling response: %v", err)
			}
			var strs []string
			for i := range result.Data.Nodes[0].DgraphUserGroup {
				strs = append(strs, result.Data.Nodes[0].DgraphUserGroup[i].DgraphXid)
			}
			nodeCollection = append(nodeCollection, strs)
		}
		var saveIndex int
		prevLen := 0

		fmt.Printf("keeping group%v with uid: %v", group, uids[saveIndex])
		if group == guardianGroup {
			for k, nodes := range nodeCollection {
				if contains(nodes, "groot") && len(nodes) > prevLen {
					saveIndex = k
					prevLen = len(nodes)
				}
			}
			uniqueUsers := uniqueStringsExcluding(nodeCollection, uids)
			if err := addUsersToGroup(hc, uniqueUsers, uids[saveIndex]); err != nil {
				return err
			}
			if err := deleteUids(hc, uids, saveIndex, group); err != nil {
				return err
			}

		} else {
			if err := deleteUids(hc, uids, 0, group); err != nil {
				return err
			}
		}

	}
	return nil
}

func deleteDuplicatesUser(hc *dgraphapi.HTTPClient, duplicates map[string][]string) error {
	query := `{
		nodes(func: uid(%v)) {
			uid
			dgraph.xid  
			dgraph.type
			dgraph.user.group{
				dgraph.xid  
			}
		}
	}`
	for user, uids := range duplicates {
		var groupsCollection [][]string
		for _, uid := range uids {
			resp, err := hc.PostDqlQuery(fmt.Sprintf(query, uid))
			if err != nil {
				return err
			}
			var result Response
			if err := json.Unmarshal(resp, &result); err != nil {
				log.Fatalf("while unmarshalling response: %v", err)
			}
			var strs []string
			for i := range result.Data.Nodes[0].DgraphUserGroup {
				strs = append(strs, result.Data.Nodes[0].DgraphUserGroup[i].DgraphXid)
			}
			groupsCollection = append(groupsCollection, strs)
		}
		var saveIndex int
		prevLen := 0
		for k, groups := range groupsCollection {
			if contains(groups, "guardians") && len(groups) > prevLen {
				saveIndex = k
				prevLen = len(groups)
			}
		}

		fmt.Printf("keeping user%v with uid: %v", user, uids[saveIndex])

		if err := deleteUids(hc, uids, saveIndex, user); err != nil {
			return err
		}

	}
	return nil
}

func deleteDuplicatesUserGroup(hc *dgraphapi.HTTPClient, duplicates map[string][]string) error {
	// we will delete only user in this case
	query := `{
		nodes(func: uid(%v)) {
			uid
			dgraph.xid  
			dgraph.type
		}
	}`

	for userGroup, uids := range duplicates {
		var saveIndex int

		for i, uid := range uids {
			resp, err := hc.PostDqlQuery(fmt.Sprintf(query, uid))
			if err != nil {
				return err
			}
			var result Response
			if err := json.Unmarshal(resp, &result); err != nil {
				log.Fatalf("while unmarshalling response: %v", err)
			}

			if result.Data.Nodes[0].DgraphType[0] == "dgraph.type.group" {
				saveIndex = i
				break
			}
		}
		fmt.Printf("keeping group%v with uid: %v", userGroup, uids[saveIndex])
		fmt.Print("\n")

		if err := deleteUids(hc, uids, saveIndex, userGroup); err != nil {
			return err
		}

	}
	return nil
}

func init() {
	CheckUpgrade.Cmd = &cobra.Command{
		Use:   "checkupgrade",
		Short: "Run the checkupgrade tool",
		Long:  "The checkupgrade tool is used to check for duplicate dgraph.xid's in the Dgraph database before upgrade.",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
		Annotations: map[string]string{"group": "tool"},
	}
	CheckUpgrade.Cmd.SetHelpTemplate(x.NonRootTemplate)
	flag := CheckUpgrade.Cmd.Flags()
	flag.String(alphaHttp, "127.0.0.1:8080", "Dgraph Alpha Http server address")
	flag.String(namespace, "", "Namespace to check for duplicate nodes")
	flag.String(dgUser, "groot", "Username of the namespace's user")
	flag.String(password, "password", "Password of the namespace's user")
	flag.String(aclSecretKeyFilePath, "", "path of file that stores secret key or private key,"+
		" which is used to sign the ACL JWT")
	flag.String(jwtAlg, "HS256", "JWT signing algorithm")
	flag.String(deleteDup, "false", "set this flag to true to delete duplicates nodes")
}

func run() {
	if err := checkUpgrade(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func checkUpgrade() error {
	fmt.Println("Running check-upgrade tool")

	cmdInput := parseInput()
	var accessJwt string
	var err error
	if cmdInput.aclSecretKeyFilePath != "" {
		accessJwt, err = getAccessJwt(dgraphapi.DefaultUser, guardianGroup, 0, cmdInput.aclSecretKeyFilePath,
			cmdInput.jwtAlg)
		if err != nil {
			return fmt.Errorf("while getting access jwt token: %w", err)
		}
	}
	hc, err := setupClient(cmdInput.alphaHttp)
	if err != nil {
		return fmt.Errorf("while setting up clients: %w", err)
	}

	hc.AccessJwt = accessJwt

	var namespaces []uint64
	if cmdInput.namespace == 0 {
		namespaces, err = hc.ListNamespaces()
		if err != nil {
			return fmt.Errorf("while lisiting namespaces: %w", err)
		}
	} else {
		namespaces = append(namespaces, cmdInput.namespace)
	}

	for _, ns := range namespaces {
		if cmdInput.aclSecretKeyFilePath != "" {
			hc.AccessJwt, err = getAccessJwt(dgraphapi.DefaultUser, guardianGroup, ns, cmdInput.aclSecretKeyFilePath,
				cmdInput.jwtAlg)
			if err != nil {
				return fmt.Errorf("while getting access jwt token for namespace %v: %w", ns, err)
			}
		} else {
			if err := hc.LoginIntoNamespace(cmdInput.dgUser, cmdInput.password, ns); err != nil {
				return fmt.Errorf("while logging into namespace %v: %w", ns, err)
			}
		}

		duplicates, err := queryDuplicateNodes(hc)
		if err != nil {
			return err
		}
		if err := printAndDeleteDuplicates(hc, "user", ns, duplicates[0], cmdInput.dupDelete); err != nil {
			return err
		}
		// example output:
		//	Found duplicate users in namespace: #0
		// dgraph.xid user1 , Uids: [0x4 0x3]
		if err := printAndDeleteDuplicates(hc, "group", ns, duplicates[1], cmdInput.dupDelete); err != nil {
			return err
		}
		// Found duplicate groups in namespace: #1
		// dgraph.xid group1 , Uids: [0x2714 0x2711]
		if err := printAndDeleteDuplicates(hc, "groups and user", ns, duplicates[2], cmdInput.dupDelete); err != nil {
			return err
		}
		// Found duplicate groups and users in namespace: #0
		// dgraph.xid userGroup1 , Uids: [0x7532 0x7531]
	}

	fmt.Println("To delete duplicate nodes use following mutation: ")
	deleteMut := `
	{
		delete {
			<UID> * * .
		}
	}`
	fmt.Fprint(os.Stderr, deleteMut)
	return nil
}

func parseInput() *commandInput {
	return &commandInput{alphaHttp: CheckUpgrade.Conf.GetString(alphaHttp), dgUser: CheckUpgrade.Conf.GetString(dgUser),
		password: CheckUpgrade.Conf.GetString(password), namespace: CheckUpgrade.Conf.GetUint64(namespace),
		aclSecretKeyFilePath: CheckUpgrade.Conf.GetString(aclSecretKeyFilePath),
		jwtAlg:               CheckUpgrade.Conf.GetString(jwtAlg), dupDelete: CheckUpgrade.Conf.GetBool(deleteDup)}
}
