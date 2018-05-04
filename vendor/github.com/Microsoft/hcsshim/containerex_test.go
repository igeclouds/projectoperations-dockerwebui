// +build windows

//
// These unit tests must run on a system setup to run both Argons and Xenons,
// have docker installed, and have the nanoserver (WCOW) and alpine (LCOW)
// base images installed. The nanoserver image MUST match the build of the
// host.
//
// We rely on docker as the tools to extract a container image aren't
// open source. We use it to find the location of the base image on disk.
//

package hcsshim

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	//	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

var (
	// Obtained from docker - for the base images used in the tests
	//	nanoImagePath    string
	alpineImagePath string
	//busyboxImagePath string
	//busyboxROLayers  []string
	layersNanoserver []string
	layersBusybox    []string // github.com/jhowardmsft/busybox. Just an arbitrary multi-layer iamge  // TODO We could build a simple image in here.

	cacheSandboxFile     = ""      // LCOW ext4 sandbox file
	cacheSandboxDir      = ""      // LCOW ext4 sandbox directory
	lcowServiceContainer Container // For generating LCOW ext4 sandbox
)

func init() {
	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000000000Z07:00",
		FullTimestamp:   true,
	})

	os.Setenv("HCSSHIM_LCOW_DEBUG_ENABLE", "something")
	layersNanoserver = getLayers("microsoft/nanoserver:latest")
	layersBusybox = getLayers("microsoft/windowsservercore")
}

func getLayerChain(layerFolder string) []string {
	jPath := filepath.Join(layerFolder, "layerchain.json")
	content, err := ioutil.ReadFile(jPath)
	if os.IsNotExist(err) {
		panic("layerchain not found")
	} else if err != nil {
		panic("failed to read layerchain")
	}

	var layerChain []string
	err = json.Unmarshal(content, &layerChain)
	if err != nil {
		panic("failed to unmarshal layerchain")
	}
	return layerChain
}

func getLayers(imageName string) []string {
	cmd := exec.Command("docker", "inspect", imageName, "-f", `"{{.GraphDriver.Data.dir}}"`)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		panic("failed to get layers. Is the daemon running?")
	}
	imagePath := strings.Replace(strings.TrimSpace(out.String()), `"`, ``, -1)
	layers := getLayerChain(imagePath)
	return append([]string{imagePath}, layers...)
}

// createTempDir creates a temporary directory for use by a container.
func createTempDir(t *testing.T) string {
	tempDir, err := ioutil.TempDir("", "hcsshimtestcase")
	if err != nil {
		t.Fatalf("failed to create temporary directory: %s", err)
	}
	return tempDir
}

// TODO Make this more a public function.
// createWCOWTempDirWithSandbox uses HCS to create a sandbox with VM group access
// in a temporary directory. Returns the directory, the "containerID" which is
// really the foldername where the sandbox is, and a constructed DriverInfo
// structure which is required for calling v1 APIs. Strictly VM group access is
// not required for an argon.
func createWCOWTempDirWithSandbox(t *testing.T) string {
	tempDir := createTempDir(t)
	di := DriverInfo{HomeDir: filepath.Dir(tempDir)}
	if err := CreateSandboxLayer(di, filepath.Base(tempDir), filepath.Base(layersNanoserver[0]), layersNanoserver[:1]); err != nil {
		t.Fatalf("Failed CreateSandboxLayer: %s", err)
	}
	return tempDir
}

// createLCOWTempDirWithSandbox uses an LCOW utility VM to create a blank
// VHDX and format it ext4.
func createLCOWTempDirWithSandbox(t *testing.T) (string, string) {
	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	options[HCSOPTION_ID] = "global"
	options[HCSOPTION_OWNER] = "unit-test"
	if lcowServiceContainer == nil {
		cacheSandboxDir = createTempDir(t)
		t.Logf("Creating an LCOW service VM")
		var err error
		lcowServiceContainer, err = CreateContainerEx(&CreateOptions{
			Options:     options,
			Logger:      logrus.WithField("module", "hcsshim unit test"),
			Spec:        defaultLinuxSpec(),
			LCOWOptions: getLCOWOptions(),
		})
		if err != nil {
			t.Fatalf("Failed create: %s", err)
		}
		if err := lcowServiceContainer.Start(); err != nil {
			t.Fatalf("Failed to start service container: %s", err)
		}
	}
	t.Logf("Creating EXT4 sandbox for LCOW test cases")
	tempDir := createTempDir(t)
	cacheSandboxFile = filepath.Join(cacheSandboxDir, "sandbox.vhdx")
	if err := lcowServiceContainer.CreateExt4Vhdx(filepath.Join(tempDir, "sandbox.vhdx"), DefaultLCOWVhdxSizeGB, cacheSandboxFile); err != nil {
		t.Fatalf("failed to create EXT4 sandbox for LCOW test cases: %s", err)
	}
	return tempDir, filepath.Base(tempDir)
}

func getLCOWOptions() *LCOWOptions {
	base := filepath.Join(os.Getenv("ProgramFiles"), "Linux Containers")
	return &LCOWOptions{
		KirdPath:   base,
		KernelFile: "bootx64.efi",
		InitrdFile: "initrd.img",
	}
}

func startContainer(t *testing.T, c Container) {
	if err := c.Start(); err != nil {
		t.Fatalf("Failed start: %s", err)
	}
}

// Helper to launch a process in it. At the
// point of calling, the container must have been successfully created.
func runCommand(t *testing.T, c Container, command, workdir, expectedOutput string) {
	if c == nil {
		t.Fatalf("requested container to start is nil!")
	}
	p, err := c.CreateProcess(&ProcessConfig{
		CommandLine:      command,
		WorkingDirectory: workdir,
		CreateStdInPipe:  true,
		CreateStdOutPipe: true,
		CreateStdErrPipe: true,
	})
	if err != nil {
		//		c.DebugLCOWGCS()
		//		time.Sleep(60 * time.Minute)
		t.Fatalf("Failed Create Process: %s", err)

	}
	defer p.Close()
	if err := p.Wait(); err != nil {
		t.Fatalf("Failed Wait Process: %s", err)
	}
	exitCode, err := p.ExitCode()
	if err != nil {
		t.Fatalf("Failed to obtain process exit code: %s", err)
	}
	if exitCode != 0 {
		t.Fatalf("Non-zero exit code from process %s (%d)", command, exitCode)
	}
	_, o, _, err := p.Stdio()
	if err != nil {
		t.Fatalf("Failed to get Stdio handles for process: %s", err)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(o)
	out := strings.TrimSpace(buf.String())
	if out != expectedOutput {
		t.Fatalf("Failed to get %q from process: %q", expectedOutput, out)
	}
}

// Helper to stop a container
func stopContainer(t *testing.T, c Container) {
	if err := c.Shutdown(); err != nil {
		if IsPending(err) {
			if err := c.Wait(); err != nil {
				t.Fatalf("Failed Wait shutdown: %s", err)
			}
		} else {
			t.Fatalf("Failed shutdown: %s", err)
		}
	}
	//c.Terminate()
}

func iPtr(i int64) *int64 { return &i }

func defaultCapabilities() []string {
	return []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}
}

// defaultLinuxSpec create a default spec for running Linux containers
// Note this is copied from moby/moby, but we can't use it as a package
// import as it would be circular.
func defaultLinuxSpec() *specs.Spec {
	s := &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    defaultCapabilities(),
				Permitted:   defaultCapabilities(),
				Inheritable: defaultCapabilities(),
				Effective:   defaultCapabilities(),
			},
		},
		Root: &specs.Root{},
	}
	s.Mounts = []specs.Mount{
		{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/dev",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/dev/pts",
			Type:        "devpts",
			Source:      "devpts",
			Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
		},
		{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "ro"},
		},
		{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"ro", "nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/dev/mqueue",
			Type:        "mqueue",
			Source:      "mqueue",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/dev/shm",
			Type:        "tmpfs",
			Source:      "shm",
			Options:     []string{"nosuid", "noexec", "nodev", "mode=1777"},
		},
	}

	s.Linux = &specs.Linux{
		MaskedPaths: []string{
			"/proc/kcore",
			"/proc/keys",
			"/proc/latency_stats",
			"/proc/timer_list",
			"/proc/timer_stats",
			"/proc/sched_debug",
			"/proc/scsi",
			"/sys/firmware",
		},
		ReadonlyPaths: []string{
			"/proc/asound",
			"/proc/bus",
			"/proc/fs",
			"/proc/irq",
			"/proc/sys",
			"/proc/sysrq-trigger",
		},
		Namespaces: []specs.LinuxNamespace{
			{Type: "mount"},
			{Type: "network"},
			{Type: "uts"},
			{Type: "pid"},
			{Type: "ipc"},
		},
		// Devices implicitly contains the following devices:
		// null, zero, full, random, urandom, tty, console, and ptmx.
		// ptmx is a bind mount or symlink of the container's ptmx.
		// See also: https://github.com/opencontainers/runtime-spec/blob/master/config-linux.md#default-devices
		Devices: []specs.LinuxDevice{},
		Resources: &specs.LinuxResources{
			Devices: []specs.LinuxDeviceCgroup{
				{
					Allow:  false,
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(1),
					Minor:  iPtr(5),
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(1),
					Minor:  iPtr(3),
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(1),
					Minor:  iPtr(9),
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(1),
					Minor:  iPtr(8),
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(5),
					Minor:  iPtr(0),
					Access: "rwm",
				},
				{
					Allow:  true,
					Type:   "c",
					Major:  iPtr(5),
					Minor:  iPtr(1),
					Access: "rwm",
				},
				{
					Allow:  false,
					Type:   "c",
					Major:  iPtr(10),
					Minor:  iPtr(229),
					Access: "rwm",
				},
			},
		},
	}

	// For LCOW support, populate a blank Windows spec
	if runtime.GOOS == "windows" {
		s.Windows = &specs.Windows{}
	}

	return s
}

// -------------------
//
//
// Start of test cases
//
//
// -------------------

// A v1 Argon with a single base layer
func TestV1Argon(t *testing.T) {
	t.Skip("fornow")
	tempDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	layers := append(layersNanoserver, tempDir)
	mountPath, err := Mount(layers, nil, SchemaV10())
	if err != nil {
		t.Fatalf("failed to mount container storage: %s", err)
	}
	defer Unmount(layers, nil, SchemaV10(), UnmountOperationAll)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	options[HCSOPTION_ID] = "TestV1Argon"
	options[HCSOPTION_OWNER] = "unit-test"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Hostname: "v1argontest",
			Windows:  &specs.Windows{LayerFolders: layers},
			Root:     &specs.Root{Path: mountPath.(string)},
		},
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
	stopContainer(t, c)
	c.Terminate()
}

// A v1 WCOW Xenon with a single base layer
func TestV1XenonWCOW(t *testing.T) {
	t.Skip("for now")
	tempDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	options[HCSOPTION_ID] = "TestV1XenonWCOW"
	options[HCSOPTION_OWNER] = "unit-test"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{
				LayerFolders: append(layersNanoserver, tempDir),
				HyperV:       &specs.WindowsHyperV{UtilityVMPath: filepath.Join(layersNanoserver[0], `UtilityVM`)},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
	stopContainer(t, c)
}

// A v1 WCOW Xenon with a single base layer but let HCSShim find the utility VM path
func TestV1XenonWCOWNoUVMPath(t *testing.T) {
	t.Skip("for now")
	tempDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	options[HCSOPTION_ID] = "TestV1XenonWCOWNoUVMPath"
	options[HCSOPTION_OWNER] = "unit-test"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{
				LayerFolders: append(layersNanoserver, tempDir),
				HyperV:       &specs.WindowsHyperV{},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
	stopContainer(t, c)
}

// A v1 LCOW
// TODO LCOW doesn't work currently
func TestV1XenonLCOW(t *testing.T) {
	t.Skip("for now")
	tempDir, _ := createLCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	options[HCSOPTION_ID] = "TestV1XenonLCOW"
	options[HCSOPTION_OWNER] = "unit-test"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{LayerFolders: []string{alpineImagePath, tempDir}},
			Linux:   &specs.Linux{},
		},
		LCOWOptions: getLCOWOptions(),
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "echo Hello", `/bin`, "Hello")
	stopContainer(t, c)
	c.Terminate()
}

// A v2 Argon with a single base layer
func TestV2Argon(t *testing.T) {
	//	t.Skip("fornow")
	tempDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	layers := append(layersNanoserver, tempDir)
	mountPath, err := Mount(layers, nil, SchemaV20())
	if err != nil {
		t.Fatalf("failed to mount container storage: %s", err)
	}
	defer Unmount(layers, nil, SchemaV20(), UnmountOperationAll)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_ID] = "TestV2Argon"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Hostname: "v2argontest",
			Windows:  &specs.Windows{LayerFolders: layers},
			Root:     &specs.Root{Path: mountPath.(string)},
		},
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
	stopContainer(t, c)
	c.Terminate()
}

// A v2 Argon with multiple layers
func TestV2ArgonMultipleBaseLayers(t *testing.T) {
	t.Skip("fornow")
	tempDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(tempDir)

	layers := append(layersBusybox, tempDir)
	mountPath, err := Mount(layers, nil, SchemaV20())
	if err != nil {
		t.Fatalf("failed to mount container storage: %s", err)
	}
	defer Unmount(layers, nil, SchemaV20(), UnmountOperationAll)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_ID] = "TestV2ArgonMultipleBaseLayers"
	c, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Hostname: "v2argonmltest",
			Windows:  &specs.Windows{LayerFolders: layers},
			Root:     &specs.Root{Path: mountPath.(string)},
		},
	})
	if err != nil {
		t.Fatalf("Failed create: %s", err)
	}
	startContainer(t, c)
	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
	stopContainer(t, c)
	c.Terminate()
}

// Two v2 WCOW containers in the same UVM, each with a single base layer
// TODO: Unmounting
func TestV2XenonWCOWTwoContainers(t *testing.T) {
	t.Skip("Skipping for now")
	uvmID := "TestV2XenonWCOWTwoContainers_UVM"
	uvmScratchDir, err := ioutil.TempDir("", "hcsshimtestcase")
	if err != nil {
		t.Fatalf("Failed create temporary directory: %s", err)
	}
	if err := CreateWCOWUVMSandbox(layersNanoserver[0], uvmScratchDir, uvmID); err != nil {
		t.Fatalf("Failed create Windows UVM Sandbox: %s", err)
	}
	defer os.RemoveAll(uvmScratchDir)

	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_SPEC_DEFINES_UTILITY_VM] = "yes"
	options[HCSOPTION_ID] = uvmID
	uvm, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{
				LayerFolders: []string{uvmScratchDir},
				HyperV:       &specs.WindowsHyperV{UtilityVMPath: filepath.Join(layersNanoserver[0], `UtilityVM\Files`)},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed create UVM: %s", err)
	}
	defer uvm.Terminate()
	if err := uvm.Start(); err != nil {
		t.Fatalf("Failed start utility VM: %s", err)
	}

	// Create a sandbox for the first hosted container, then create the container
	containerAScratchDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(containerAScratchDir)

	options = make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_ID] = "containerA"
	xenonA, err := CreateContainerEx(&CreateOptions{
		HostingSystem: uvm,
		Options:       options,
		Logger:        logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{LayerFolders: append(layersNanoserver, containerAScratchDir)},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainerEx failed: %s", err)
	}

	// Create a sandbox for the second hosted container, then create the container
	containerBScratchDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(containerBScratchDir)
	options[HCSOPTION_ID] = "containerB"
	xenonB, err := CreateContainerEx(&CreateOptions{
		HostingSystem: uvm,
		Options:       options,
		Logger:        logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{LayerFolders: append(layersNanoserver, containerBScratchDir)},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainerEx failed: %s", err)
	}

	// Start/stop both containers
	startContainer(t, xenonA)
	runCommand(t, xenonA, "cmd /s /c echo ContainerA", `c:\`, "ContainerA")
	startContainer(t, xenonB)
	runCommand(t, xenonB, "cmd /s /c echo ContainerB", `c:\`, "ContainerB")
	stopContainer(t, xenonA)
	stopContainer(t, xenonB)
	xenonA.Terminate()
	xenonB.Terminate()
}

// TODO: Test UVMResourcesFromContainerSpec
func TestUVMSizing(t *testing.T) {

}

// A single WCOW xenon
func TestV2XenonWCOW(t *testing.T) {
	//t.Skip("Skipping for now")
	uvmID := "Testv2XenonWCOW_UVM"
	uvmScratchDir, err := ioutil.TempDir("", "uvmScratch")
	if err != nil {
		t.Fatalf("Failed create temporary directory: %s", err)
	}
	// TODO: Have test wrapper which calls this, but also creates a temporary directory above.
	// TODO: Stop calling it "scratch". It's really the boot/OS bit of a UVM.
	if err := CreateWCOWUVMSandbox(layersBusybox[0], uvmScratchDir, uvmID); err != nil {
		t.Fatalf("Failed create Windows UVM Sandbox: %s", err)
	}
	defer os.RemoveAll(uvmScratchDir)

	// TODO: Stefan's test created this with flags 16785. I do 16407.
	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_SPEC_DEFINES_UTILITY_VM] = "yes" // TODO CREATE_AS_UTILITY_VM
	options[HCSOPTION_ID] = uvmID                      // TODO Test to make sure this is optional
	uvm, err := CreateContainerEx(&CreateOptions{
		Options: options,
		Logger:  logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{
				LayerFolders: []string{uvmScratchDir},                                                                 // TODO JJH. Do we even need this?
				HyperV:       &specs.WindowsHyperV{UtilityVMPath: filepath.Join(layersBusybox[0], `UtilityVM\Files`)}, // This we don't need.
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed create UVM: %s", err)
	}
	defer uvm.Terminate()
	if err := uvm.Start(); err != nil {
		t.Fatalf("Failed start utility VM: %s", err)
	}

	containerScratchDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(containerScratchDir)

	// Create the container hosted inside the utility VM
	options = make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_ID] = "container"
	layerFolders := append(layersBusybox, containerScratchDir)
	xenon, err := CreateContainerEx(&CreateOptions{
		HostingSystem: uvm,
		Options:       options,
		Logger:        logrus.WithField("module", "hcsshim unit test"),
		Spec:          &specs.Spec{Windows: &specs.Windows{LayerFolders: layerFolders}},
	})
	if err != nil {
		t.Fatalf("CreateContainerEx failed: %s", err)
	}
	defer Unmount(layerFolders, uvm, SchemaV20(), UnmountOperationAll) // TODO Why have schema here? It should be known and stored.

	// Start/stop the container
	startContainer(t, xenon)
	runCommand(t, xenon, "cmd /s /c echo TestV2XenonWCOW", `c:\`, "TestV2XenonWCOW")
	stopContainer(t, xenon)
	xenon.Terminate()
}

//// This verifies the container storage is unmounted correctly so that a second
//// container can be started from the same storage.
//func TestV2XenonWCOWWithRemount(t *testing.T) {
////	t.Skip("Skipping for now")
//	uvmID := "Testv2XenonWCOWWithRestart_UVM"
//	uvmScratchDir, err := ioutil.TempDir("", "uvmScratch")
//	if err != nil {
//		t.Fatalf("Failed create temporary directory: %s", err)
//	}
//	if err := CreateWCOWSandbox(layersNanoserver[0], uvmScratchDir, uvmID); err != nil {
//		t.Fatalf("Failed create Windows UVM Sandbox: %s", err)
//	}
//	defer os.RemoveAll(uvmScratchDir)

//	uvm, err := CreateContainerEx(&CreateOptions{
//		Id:              uvmID,
//		Owner:           "unit-test",
//		SchemaVersion:   SchemaV20(),
//		Logger:          logrus.WithField("module", "hcsshim unit test"),
//		IsHostingSystem: true,
//		Spec: &specs.Spec{
//			Windows: &specs.Windows{
//				LayerFolders: []string{uvmScratchDir},
//				HyperV:       &specs.WindowsHyperV{UtilityVMPath: filepath.Join(layersNanoserver[0], `UtilityVM\Files`)},
//			},
//		},
//	})
//	if err != nil {
//		t.Fatalf("Failed create UVM: %s", err)
//	}
//	defer uvm.Terminate()
//	if err := uvm.Start(); err != nil {
//		t.Fatalf("Failed start utility VM: %s", err)
//	}

//	// Mount the containers storage in the utility VM
//	containerScratchDir := createWCOWTempDirWithSandbox(t)
//	layerFolders := append(layersNanoserver, containerScratchDir)
//	cls, err := Mount(layerFolders, uvm, SchemaV20())
//	if err != nil {
//		t.Fatalf("failed to mount container storage: %s", err)
//	}
//	combinedLayers := cls.(CombinedLayersV2)
//	mountedLayers := &ContainersResourcesStorageV2{
//		Layers: combinedLayers.Layers,
//		Path:   combinedLayers.ContainerRootPath,
//	}
//	defer func() {
//		if err := Unmount(layerFolders, uvm, SchemaV20(), UnmountOperationAll); err != nil {
//			t.Fatalf("failed to unmount container storage: %s", err)
//		}
//	}()

//	// Create the first container
//	defer os.RemoveAll(containerScratchDir)
//	xenon, err := CreateContainerEx(&CreateOptions{
//		Id:            "container",
//		Owner:         "unit-test",
//		HostingSystem: uvm,
//		SchemaVersion: SchemaV20(),
//		Logger:        logrus.WithField("module", "hcsshim unit test"),
//		Spec:          &specs.Spec{Windows: &specs.Windows{}}, // No layerfolders as we mounted them ourself.
//	})
//	if err != nil {
//		t.Fatalf("CreateContainerEx failed: %s", err)
//	}

//	// Start/stop the first container
//	startContainer(t, xenon)
//	runCommand(t, xenon, "cmd /s /c echo TestV2XenonWCOWFirstStart", `c:\`, "TestV2XenonWCOWFirstStart")
//	stopContainer(t, xenon)
//	xenon.Terminate()

//	// Now unmount and remount to exactly the same places
//	if err := Unmount(layerFolders, uvm, SchemaV20(), UnmountOperationAll); err != nil {
//		t.Fatalf("failed to unmount container storage: %s", err)
//	}
//	if _, err = Mount(layerFolders, uvm, SchemaV20()); err != nil {
//		t.Fatalf("failed to mount container storage: %s", err)
//	}

//	// Create an identical second container and verify it works too.
//	xenon2, err := CreateContainerEx(&CreateOptions{
//		Id:            "container",
//		Owner:         "unit-test",
//		HostingSystem: uvm,
//		SchemaVersion: SchemaV20(),
//		Logger:        logrus.WithField("module", "hcsshim unit test"),
//		Spec:          &specs.Spec{Windows: &specs.Windows{LayerFolders: layerFolders}},
//		MountedLayers: mountedLayers,
//	})
//	if err != nil {
//		t.Fatalf("CreateContainerEx failed: %s", err)
//	}
//	startContainer(t, xenon2)
//	runCommand(t, xenon2, "cmd /s /c echo TestV2XenonWCOWAfterRemount", `c:\`, "TestV2XenonWCOWAfterRemount")
//	stopContainer(t, xenon2)
//	xenon2.Terminate()
//}

// TestCreateContainerExv2XenonWCOWMultiLayer creates a V2 Xenon having multiple image layers
func TestCreateContainerExv2XenonWCOWMultiLayer(t *testing.T) {
	t.Skip("for now")
	uvmID := "TestCreateContainerExv2XenonWCOWMultiLayer_UVM"
	uvmScratchDir, err := ioutil.TempDir("", "hcsshimtestcase")
	if err != nil {
		t.Fatalf("Failed create temporary directory: %s", err)
	}
	if err := CreateWCOWUVMSandbox(layersNanoserver[0], uvmScratchDir, uvmID); err != nil {
		t.Fatalf("Failed create Windows UVM Sandbox: %s", err)
	}
	defer os.RemoveAll(uvmScratchDir)

	uvmMemory := uint64(1 * 1024 * 1024 * 1024)
	uvmCPUCount := uint64(2)
	options := make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_SPEC_DEFINES_UTILITY_VM] = "yes"
	options[HCSOPTION_ID] = uvmID
	uvm, err := CreateContainerEx(&CreateOptions{
		Logger: logrus.WithField("module", "hcsshim unit test"),
		Spec: &specs.Spec{
			Windows: &specs.Windows{
				LayerFolders: []string{uvmScratchDir},
				HyperV:       &specs.WindowsHyperV{UtilityVMPath: filepath.Join(layersNanoserver[0], `UtilityVM\Files`)},
				Resources: &specs.WindowsResources{
					Memory: &specs.WindowsMemoryResources{
						Limit: &uvmMemory,
					},
					CPU: &specs.WindowsCPUResources{
						Count: &uvmCPUCount,
					},
				},
			},
		},
	})

	if err != nil {
		t.Fatalf("Failed create UVM: %s", err)
	}
	defer uvm.Terminate()
	if err := uvm.Start(); err != nil {
		t.Fatalf("Failed start utility VM: %s", err)
	}

	// Create a sandbox for the hosted container
	containerAScratchDir := createWCOWTempDirWithSandbox(t)
	defer os.RemoveAll(containerAScratchDir)

	// Create the container
	options = make(map[string]string)
	options[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	options[HCSOPTION_ID] = "containerA"
	xenon, err := CreateContainerEx(&CreateOptions{
		HostingSystem: uvm,
		Options:       options,
		Logger:        logrus.WithField("module", "hcsshim unit test"),
		Spec:          &specs.Spec{Windows: &specs.Windows{LayerFolders: append(layersBusybox, containerAScratchDir)}},
	})
	if err != nil {
		t.Fatalf("CreateContainerEx failed: %s", err)
	}

	// Start/stop the container
	startContainer(t, xenon)
	runCommand(t, xenon, "echo Container", `c:\`, "Container")
	stopContainer(t, xenon)
	xenon.Terminate()
}

// Note that the .syso file is required to manifest the test app
func TestDetermineSchemaVersion(t *testing.T) {
	m := make(map[string]string)
	if sv := determineSchemaVersion(nil); !sv.IsV20() {
		t.Fatalf("expected v2")
	}
	if sv := determineSchemaVersion(m); !sv.IsV20() {
		t.Fatalf("expected v2")
	}
	m[HCSOPTION_SCHEMA_VERSION] = SchemaV20().String()
	if sv := determineSchemaVersion(m); !sv.IsV20() {
		t.Fatalf("expected requested v2")
	}
	m[HCSOPTION_SCHEMA_VERSION] = SchemaV10().String()
	if sv := determineSchemaVersion(m); !sv.IsV10() {
		t.Fatalf("expected requested v1")
	}
	m[HCSOPTION_SCHEMA_VERSION] = (&SchemaVersion{}).String()
	if sv := determineSchemaVersion(m); !sv.IsV20() { // Should also log a warning that 0.0 is ignored
		t.Fatalf("expected requested v2")
	}
}