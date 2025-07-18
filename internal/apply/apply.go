// Package apply implements an "atomic apply" algorithm for Kubernetes resources.
//
// The public entry‑point is RunApply, which mimics the behaviour of
//
//	kubectl apply -f FILE ...
//
// but guarantees **all‑or‑nothing** semantics:
//
//  1. Each manifest is applied server‑side (SSA).
//  2. If any step fails or any resource does not reach the desired status
//     before the configured timeout, the helper rolls everything back to the
//     state observed *before* the invocation began.
//
// A high‑level flow looks like this:
//
//   - readDocs()    -> YAML/JSON -> []*unstructured.Unstructured
//   - prepareApplyPlan()         -> []applyItem (backup & CRUD plan)
//   - applyPlanned()             -> Patch/Update/Delete via dynamic client
//   - waitStatus()               -> poll until Current or timeout
//   - rollbackAndExit() on any error
//
// The package is meant to be used from a kubectl‑style CLI, therefore it
// relies on the same cli‑runtime helpers and accepts genericclioptions flags.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/aquasecurity/table"
	"github.com/hashmap-kz/katomik/internal/printer"
	"github.com/hashmap-kz/katomik/internal/utils"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/hashmap-kz/katomik/internal/resolve"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling/aggregator"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling/collector"
	pollEvent "sigs.k8s.io/cli-utils/pkg/kstatus/polling/event"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/cli-utils/pkg/object"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// applyItem is an internal representation of a single resource that should be
// applied as part of an atomic operation. It carries both the object to apply
// *and* the information required to restore the previous state (backup).
//
// Fields:
//
//	obj     - desired state decoded from the manifest
//	dr      - dynamic client scoped to the resource
//	existed - whether the object was present before the run started
//	backup  - original JSON of the resource (only if existed=true)
//	rv      - original resourceVersion (used to preserve concurrency semantics)
//
// All fields are populated by prepareApplyPlan() and consumed by applyPlanned()
// and rollbackAndExit().
//
// The struct is intentionally kept small so a slice of items can be passed
// around without heavy copying.
type applyItem struct {
	obj     *unstructured.Unstructured
	dr      dynamic.ResourceInterface
	existed bool
	backup  []byte
	rv      string
}

// AtomicApplyOptions groups the user‑visible flags of the CLI layer.
// It purposely contains **only plain data** so it can be embedded in higher
// level option structs or reused in tests.
//
//	Filenames - list of paths or "-" for stdin
//	Timeout   - maximum time to wait for resources to become Current
//	Recursive - whether to walk directories recursively when expanding -f
//	            arguments.
type AtomicApplyOptions struct {
	Filenames []string
	Timeout   time.Duration
	Recursive bool
}

// AtomicApplyRunOptions wires together everything required to run the high
// level algorithm (config flags, IO streams and the user flags above).
//
// This mirrors the pattern used by upstream kubectl commands so the calling
// code can build the struct in the same way it would for a builtin command.
//
//	ConfigFlags - kubectl connection flags (kubeconfig, context, namespace …)
//	Streams     - stdin/stdout/stderr (allows unit‑testing with fake streams)
//	ApplyOpts   - parsed AtomicApplyOptions
//
// The struct is passed *by pointer* to avoid large copies.
type AtomicApplyRunOptions struct {
	ConfigFlags *genericclioptions.ConfigFlags
	Streams     genericiooptions.IOStreams
	ApplyOpts   AtomicApplyOptions
}

// RunApply is the public entry‑point. It orchestrates the full lifecycle:
// parsing manifests, creating a plan, applying, waiting for readiness and
// rolling back on failure.
//
//	ctx     - context that can enforce an overall deadline/cancelation
//	runOpts - fully populated AtomicApplyRunOptions
//
// It returns nil on success or an error if *any* step failed. When the function
// returns an error, it is guaranteed that rollbackAndExit has *already* been
// invoked or rollback itself failed (the latter is included in the returned
// error chain).
func RunApply(ctx context.Context, runOpts *AtomicApplyRunOptions) error {
	// 1. Build REST config & clients
	cfg, err := runOpts.ConfigFlags.ToRESTConfig()
	if err != nil {
		return err
	}
	// Tune client QPS/Burst
	cfg.QPS = 50
	cfg.Burst = 100

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	crClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	// 2. Decode all manifest files or stdin
	allDocs, err := readDocs(runOpts)
	if err != nil {
		return err
	}

	// 3. Build an apply plan (detect existing objects & backup)
	plan, err := prepareApplyPlan(allDocs, mapper, runOpts, dyn)
	if err != nil {
		return err
	}

	// 4. Apply objects (SSA Patch or Create) - on *any* error rollback
	if err := applyPlanned(ctx, plan); err != nil {
		return err
	}

	// 5. Wait until every resource reaches the Current status, else rollback
	//    (ctx carries the timeout specified by the user)
	if err := waitStatus(ctx, plan, crClient, mapper); err != nil {
		return rollbackAndExit(plan)
	}

	fmt.Println("✓ Success")
	return nil
}

// applyPlanned executes the patch/create phase. For each item in the plan it
// performs a server‑side apply (PATCH with ApplyPatchType). If *any* call
// fails the function triggers a rollback *and* returns the error so the caller
// can surface it.
func applyPlanned(ctx context.Context, plan []applyItem) error {
	// contains only CRDs and Namespaces
	var stageOne []applyItem

	// contains all objects except for CRDs and Namespaces
	var stageTwo []applyItem

	// prepare stages
	for _, u := range plan {
		if utils.IsClusterDefinition(u.obj) {
			stageOne = append(stageOne, u)
		} else {
			stageTwo = append(stageTwo, u)
		}
	}

	// two stages for apply one by one
	plans := [][]applyItem{
		stageOne, stageTwo,
	}

	for _, stagePlan := range plans {
		for _, it := range stagePlan {
			objJSON, err := json.Marshal(it.obj)
			if err != nil {
				// rollback not a stage-plan, but the full
				return rollbackAndExit(plan)
			}

			// Server‑Side Apply: create or patch atomically on the apiserver.
			_, err = it.dr.Patch(
				ctx,
				it.obj.GetName(),
				types.ApplyPatchType,
				objJSON,
				metav1.PatchOptions{
					FieldManager: "atomic-apply",
					Force:        ptr.To(true), // overwrite conflicts
				},
			)
			if err != nil {
				// rollback not a stage-plan, but the full
				return rollbackAndExit(plan)
			}
		}
	}

	return nil
}

// prepareApplyPlan turns a slice of *unstructured.Unstructured into an ordered
// slice of applyItems. For each resource it figures out:
//
//   - the correct dynamic.ResourceInterface (namespaced or cluster‑scoped)
//   - whether the object already exists (GET)
//   - a JSON backup of the original object (for rollback)
//
// The returned plan preserves the order of the input manifests - this allows
// users to control apply order by structuring their kustomization/helm output
// or file list.
func prepareApplyPlan(
	allDocs []*unstructured.Unstructured,
	mapper *restmapper.DeferredDiscoveryRESTMapper,
	runOpts *AtomicApplyRunOptions,
	dyn *dynamic.DynamicClient,
) ([]applyItem, error) {
	plan := make([]applyItem, 0, len(allDocs))

	for _, u := range allDocs {
		gvk := u.GroupVersionKind()

		// Resolve GVK -> GVR
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			mapper.Reset()
			m, err = mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				return nil, fmt.Errorf("could not map GVK %v: %v", gvk, err)
			}
		}

		// Build a dynamic.ResourceInterface scoped to namespace if required
		var dr dynamic.ResourceInterface
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			if u.GetNamespace() == "" {
				ns := "default"
				if runOpts.ConfigFlags.Namespace != nil && *runOpts.ConfigFlags.Namespace != "" {
					ns = *runOpts.ConfigFlags.Namespace
				}
				u.SetNamespace(ns)
			}
			dr = dyn.Resource(m.Resource).Namespace(u.GetNamespace())
		} else {
			dr = dyn.Resource(m.Resource)
		}

		it := applyItem{obj: u, dr: dr}

		// Detect current state to enable rollback
		cur, err := dr.Get(context.TODO(), u.GetName(), metav1.GetOptions{})
		if err == nil {
			it.existed = true
			it.rv = cur.GetResourceVersion()

			// minimise diff size for backup:
			// remove fields that should *not* be compared or preserved in the
			// backup copy (status, managedFields, etc.). This keeps the backup small and
			// avoids PATCH conflicts during rollback.
			unstructured.RemoveNestedField(cur.Object, "metadata", "creationTimestamp")
			unstructured.RemoveNestedField(cur.Object, "metadata", "generation")
			unstructured.RemoveNestedField(cur.Object, "metadata", "resourceVersion")
			unstructured.RemoveNestedField(cur.Object, "metadata", "uid")
			unstructured.RemoveNestedField(cur.Object, "metadata", "managedFields")
			unstructured.RemoveNestedField(cur.Object, "status")

			it.backup, err = json.Marshal(cur.Object)
			if err != nil {
				return nil, err
			}
		}

		plan = append(plan, it)
	}

	return plan, nil
}

// readDocs resolves -f arguments (or stdin '-') into a slice of decoded
// Kubernetes objects. It expands directory globs, walks recursively if
// requested and supports YAML documents containing multiple resources.
func readDocs(runOpts *AtomicApplyRunOptions) ([]*unstructured.Unstructured, error) {
	var allDocs []*unstructured.Unstructured

	// 1. stdin mode: exactly one filename equal to "-"
	if len(runOpts.ApplyOpts.Filenames) == 1 && runOpts.ApplyOpts.Filenames[0] == "-" {
		d, err := io.ReadAll(runOpts.Streams.In)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		docs, err := utils.ReadObjects(bytes.NewReader(d))
		if err != nil {
			return nil, err
		}
		allDocs = append(allDocs, docs...)
		return allDocs, nil
	}

	// 2. file paths & directories
	files, err := resolve.ResolveAllFiles(runOpts.ApplyOpts.Filenames, runOpts.ApplyOpts.Recursive)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		fileContent, err := resolve.ReadFileContent(file)
		if err != nil {
			return nil, err
		}
		docs, err := utils.ReadObjects(bytes.NewReader(fileContent))
		if err != nil {
			return nil, err
		}
		allDocs = append(allDocs, docs...)
	}

	return allDocs, nil
}

// rollbackAndExit attempts to restore the cluster to the exact state observed
// at the start of RunApply. It iterates over the plan *in the same order* and
// either restores the backup JSON or deletes newly created objects.
//
// If rollback succeeds the process terminates with os.Exit(1). If rollback
// itself fails, the function returns the error so the caller can propagate it.
func rollbackAndExit(plan []applyItem) error {
	fmt.Println("⟲ rollback ...")

	for _, it := range plan {
		if it.existed {
			// Recreate the previous version from the JSON backup.
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(it.backup); err != nil {
				return err
			}
			if _, err := it.dr.Update(context.TODO(), u, metav1.UpdateOptions{}); err != nil {
				return err
			}
		} else {
			if err := it.dr.Delete(context.TODO(), it.obj.GetName(), metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
	}

	// TODO: revive workloads (e.g. restart deployments) if needed

	fmt.Println("rollback complete")
	os.Exit(1) // terminate so caller does not continue after fatal failure
	return nil // unreachable but required by compiler
}

// waitStatus polls every resource in the plan until they all reach the desired
// kstatus.CurrentStatus (READY/AVAILABLE). It builds on cli‑utils status
// poller so behaviour matches kubectl‑apply‑describe.
//
// The function cancels its internal poller when either:
//
//	a) every resource is Current, or
//	b) the outer context (with timeout) expires.
func waitStatus(
	ctx context.Context,
	plan []applyItem,
	reader ctrlclient.Reader,
	mapper meta.RESTMapper,
) error {
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 1. Convert applyItems -> ObjMetadata list
	resources := make([]object.ObjMetadata, 0, len(plan))
	for _, it := range plan {
		id, err := object.RuntimeToObjMeta(it.obj)
		if err != nil {
			return err
		}
		resources = append(resources, id)
	}

	if len(resources) == 0 {
		fmt.Println("✓ no trackable resources")
		return nil
	}

	// print waiting resources
	calcLen := printer.CalcLen(resources)
	t := table.New(os.Stdout)
	t.SetRowLines(false)
	t.SetHeaders("RESOURCE", "NAMESPACE")
	for _, id := range resources {
		ns := id.Namespace
		if ns == "" {
			ns = "(cluster)"
		}
		kn := fmt.Sprintf("%s/%s", id.GroupKind.Kind, id.Name)
		t.AddRow(kn, ns)
	}
	t.Render()

	fmt.Println("+ watching")

	// 2. Start status poller
	poller := polling.NewStatusPoller(reader, mapper, polling.Options{})
	eventCh := poller.Poll(cancelCtx, resources, polling.PollOptions{PollInterval: 2 * time.Second})

	// 3. Listen & aggregate
	statusCollector := collector.NewResourceStatusCollector(resources)
	done := statusCollector.ListenWithObserver(eventCh, statusObserver(cancel, kstatus.CurrentStatus, calcLen))
	<-done

	fmt.Println("+ watching")

	// 4. "Global" error emitted by collector
	if statusCollector.Error != nil {
		return statusCollector.Error
	}

	// 5. Outer context deadline reached?
	if ctx.Err() != nil {
		var errs []error
		for _, id := range resources {
			rs := statusCollector.ResourceStatuses[id]
			if rs != nil && rs.Status != kstatus.CurrentStatus {
				errs = append(errs, fmt.Errorf("resource not ready: %s (%s)", id.String(), rs.Status))
			}
		}
		errs = append(errs, ctx.Err())
		return errors.Join(errs...)
	}

	return nil
}

// statusObserver prints a single line with the *first* non‑ready resource and
// cancels the poller when the aggregate state matches the desired one.
func statusObserver(cancel context.CancelFunc, desired kstatus.Status, calcLen *printer.Len) collector.ObserverFunc {
	printed := make(map[object.ObjMetadata]struct{})

	return func(c *collector.ResourceStatusCollector, _ pollEvent.Event) {
		var rss []*pollEvent.ResourceStatus
		var nonReady []*pollEvent.ResourceStatus

		for _, rs := range c.ResourceStatuses {
			if rs == nil {
				continue
			}
			if rs.Status == kstatus.UnknownStatus && desired == kstatus.NotFoundStatus {
				continue
			}
			rss = append(rss, rs)
			if rs.Status != desired {
				nonReady = append(nonReady, rs)
			}
		}

		// Aggregate over all resources
		if aggregator.AggregateStatus(rss, desired) == desired {
			cancel()
			return
		}

		// Log first non‑ready object (friendly UX)
		if len(nonReady) > 0 {
			sort.Slice(nonReady, func(i, j int) bool {
				return nonReady[i].Identifier.Name < nonReady[j].Identifier.Name
			})
			first := nonReady[0]

			// wait message
			id := first.Identifier
			if _, ok := printed[id]; !ok {
				ns := id.Namespace
				if ns == "" {
					ns = "(cluster)"
				}
				kn := fmt.Sprintf("%s/%s", id.GroupKind.Kind, id.Name)
				fmt.Printf("| %-*s %-*s %s\n",
					calcLen.KindNameMaxLen,
					kn,
					calcLen.NamespaceMaxLen,
					ns,
					first.Status,
				)
				printed[id] = struct{}{}
			}
		}
	}
}
