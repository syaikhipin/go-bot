package plugins

import (
	"encoding/json"
	"go-bot/plugin"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparationActivatesSuccessfulPackageAndSkipsItsReinstallation(t *testing.T) {
	c := testRuntimeConfig(t)
	c.Workspace = t.TempDir()
	directory := t.TempDir()
	root := filepath.Join(directory, "profile", "node_modules", "fixture")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"package.json": `{"name":"fixture","version":"0.1.0","type":"module","exports":"./index.mjs","goBot":{"api":"1"}}`,
		"index.mjs":    `export const inject=['host'];export async function apply(ctx){await ctx.host.tools.register({name:'echo',description:'Ready',inputSchema:{type:'object'},async execute(){return{content:[{type:'text',text:'ready'}]}}})}`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(directory, "installed.json")
	count := filepath.Join(directory, "installs")
	_ = os.WriteFile(state, []byte("[]"), 0600)
	paths, _ := json.Marshal([]string{state, count})
	main := filepath.Join(directory, "packages.mjs")
	script := `import{readFile,writeFile,appendFile}from'node:fs/promises';const[state,count]=` + string(paths) + `;
let text='';for await(const chunk of process.stdin)text+=chunk;const req=JSON.parse(text);let installed=JSON.parse(await readFile(state,'utf8'));let result=installed;
if(req.method==='install'){
 if(req.name==='bad'){process.stdout.write(JSON.stringify({error:{code:'INSTALL_FAILED',message:'intentional download failure'}}));process.exit(0)}
 result={name:req.name,version:'0.1.0',api:'1',present:true,source:req.source};installed.push(result);await writeFile(state,JSON.stringify(installed));await appendFile(count,'installed\n')
}process.stdout.write(JSON.stringify({result}));`
	if err := os.WriteFile(main, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.Context(), ManagerConfig{Directory: directory, Runtime: c, PackageMain: main})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := plugin.RuntimeSettings{Packages: map[string]plugin.RuntimePackage{
		"fixture": {Source: plugin.Source{Type: "directory", Spec: root}}, "bad": {Source: plugin.Source{Type: "directory", Spec: root}},
	}, Instances: []plugin.InstanceConfig{{ID: "fixture", Package: "fixture", Enabled: true}, {ID: "bad", Package: "bad", Enabled: true}}}
	for range 2 {
		if _, err := m.Apply(t.Context(), s); err == nil || !strings.Contains(err.Error(), "intentional download failure") {
			t.Fatalf("missing failure: %v", err)
		}
		tools, err := m.Tools(t.Context(), "session")
		if err != nil || len(tools) != 1 || tools[0].Spec().Name != "fixture__echo" {
			t.Fatalf("successful package not usable: %v %v", tools, err)
		}
	}
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "installed\n" {
		t.Fatalf("successful install reran: %q %v", data, err)
	}
}
