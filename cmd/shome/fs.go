package main

import (
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davidwu/shome/internal/ctl"
)

// `shome fs` manages a user's files across the cluster.
//
// The model it exposes, and the reason it exists: there is no shared
// filesystem. A file lives on one machine, and the same relative path on two
// machines is two files costing two lots of space. Slurm hides this behind a
// mounted home; shome cannot, so every path here names a machine.
//
//	shome fs ls                      everything, everywhere
//	shome fs ls mini:                one machine
//	shome fs cp mini:data.bin gpu:   copy between machines
//	shome fs mv mini:old.bin gpu:    copy, then delete the original
//	shome fs sync mini: gpu:         make gpu match mini
//	shome fs du                      where your space has gone
//
// Listings come from the controller's index, which is what each machine last
// reported. That is a cache and is labelled as one: waiting for a round trip
// to every machine would make `ls` as slow as the slowest node, and
// impossible when one is asleep.

func fsUsage() {
	fmt.Print(`shome fs - your files, across the cluster

  fs ls [NODE:][PATH]        list files
  fs du                      space used per machine, and your total
  fs cp SRC DST              copy between machines, or in and out
  fs mv SRC DST              copy, then remove the original
  fs sync SRC DST            make DST match SRC (one way); either side may
                             be a local directory
  fs rm [NODE:]PATH [-r]     delete

Paths are NODE:PATH, because there is no shared filesystem -- a file lives on
one machine. Omit the node in 'ls' and 'rm' to mean every machine.

  shome fs cp mini:model.bin gpu-box:
  shome fs cp mini:model.bin gpu-box:models/model.bin
  shome fs sync mini:data gpu-box:data

A side with no machine is a file on the computer you are typing on -- that is
how files get in and out.

  shome fs cp ./model.bin mini:        upload
  shome fs cp mini:result.csv .        download
  shome fs cp ./dataset mini:          upload a whole directory
  shome fs sync ./dataset mini:dataset send only what changed

A copy costs its size again on the destination, and counts against one
cluster-wide limit. 'fs du' shows where it has gone.
`)
}

// ref is a NODE:PATH reference.
type ref struct {
	Node string
	Path string

	// Local is the path exactly as it was typed, for a reference with no
	// machine.
	//
	// Path cannot serve: a path with no machine means "this path on every
	// machine" to ls and rm, which strips the leading separator so it can be
	// matched against the index. The same spelling means a file on this
	// computer to cp, mv and sync, where /tmp/data and tmp/data are two
	// different places -- and stripping it turned an absolute path into one
	// relative to the current directory, which then held nothing.
	Local string
}

// parseRef splits NODE:PATH, tolerating a bare path.
//
// A trailing colon means "this machine, at the top of my storage", which is
// what makes `cp a:file b:` read naturally.
func parseRef(s string) (ref, error) {
	if s == "" {
		return ref{}, fmt.Errorf("empty path")
	}
	node, path, found := strings.Cut(s, ":")
	if !found {
		return ref{Path: strings.TrimPrefix(s, "/"), Local: s}, nil
	}
	if node == "" {
		return ref{}, fmt.Errorf("%q has no machine before the colon", s)
	}
	return ref{Node: node, Path: strings.TrimPrefix(path, "/")}, nil
}

func (r ref) String() string {
	if r.Node == "" {
		return r.Local
	}
	return r.Node + ":" + r.Path
}

func fsCmd(args []string) error {
	if len(args) == 0 {
		return fsDu(nil)
	}
	switch args[0] {
	case "ls", "list":
		return fsLs(args[1:])
	case "du", "usage":
		return fsDu(args[1:])
	case "cp", "copy":
		return fsCopy(args[1:], false)
	case "mv", "move":
		return fsCopy(args[1:], true)
	case "sync":
		return fsSync(args[1:])
	case "rm", "remove":
		return fsRm(args[1:])
	case "help", "-h", "--help":
		fsUsage()
		return nil
	}
	fsUsage()
	return fmt.Errorf("unknown: shome fs %s", args[0])
}

func fsLs(args []string) error {
	target := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown option %q for 'shome fs ls'", a)
		}
		target = a
	}
	// No argument at all means everything, everywhere -- the documented
	// behaviour, and the first thing anyone types.
	var r ref
	if target != "" {
		var err error
		if r, err = parseRef(target); err != nil {
			return err
		}
	}
	q := "/fs?"
	if r.Node != "" {
		q += "node=" + urlEscape(r.Node) + "&"
	}
	if r.Path != "" {
		q += "path=" + urlEscape(r.Path)
	}
	var entries []ctl.FSEntry
	if err := call("GET", q, nil, &entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		if target == "" {
			fmt.Println("no files in the cluster, according to what each")
			fmt.Println("machine last reported -- a machine that is offline, or")
			fmt.Println("one that has not finished its first scan, shows nothing.")
			fmt.Println("\nput something there with:  shome fs cp <local-file> <node>:")
			return nil
		}
		fmt.Printf("nothing under %s\n", r)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tSIZE\tMODIFIED\tPATH")
	for _, e := range entries {
		when := e.MTime
		if len(when) > 19 {
			when = strings.Replace(when[:19], "T", " ", 1)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Node, bytesShort(e.Size), when, e.Path)
	}
	w.Flush()
	fmt.Printf("\n%d file(s). This is the index each machine last reported.\n", len(entries))
	return nil
}

func fsDu(args []string) error {
	q := "/fs/usage"
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown option %q for 'shome fs du'", a)
		}
		q += "?user=" + urlEscape(a)
	}
	var u ctl.FSUsage
	if err := call("GET", q, nil, &u); err != nil {
		return err
	}
	if len(u.Nodes) == 0 {
		fmt.Printf("%s has no files on any machine that has reported in\n", u.User)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tUSED\tFILES\tAS OF")
	sort.Slice(u.Nodes, func(i, j int) bool { return u.Nodes[i].Bytes > u.Nodes[j].Bytes })
	for _, n := range u.Nodes {
		when := n.AsOf
		if len(when) > 19 {
			when = strings.Replace(when[:19], "T", " ", 1)
		}
		if n.Stale {
			when += " (stale)"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", n.Node, bytesShort(n.Bytes), n.Files, when)
	}
	fmt.Fprintf(w, "\t\t\t\n")
	limit := "unlimited"
	if u.LimitBytes >= 0 {
		limit = bytesShort(u.LimitBytes)
	}
	fmt.Fprintf(w, "TOTAL\t%s\t%d\tof %s\n", bytesShort(u.TotalBytes), u.Files, limit)
	w.Flush()

	// The thing users have to understand about a cluster with no shared
	// filesystem, said where they will see it.
	if len(u.Nodes) > 1 {
		fmt.Printf("\nStorage is per machine and adds up: the same file on two machines\n")
		fmt.Printf("costs twice, and counts twice against your limit.\n")
	}
	if u.LimitBytes >= 0 && u.TotalBytes > u.LimitBytes/10*9 {
		fmt.Printf("\n%s\n", "You are close to your limit. Free space with: shome fs rm")
	}
	if u.Truncated {
		fmt.Printf("\nSome listings are partial (too many files to index), but these\n")
		fmt.Printf("totals are exact -- they are measured, not counted from the index.\n")
	}
	return nil
}

func fsCopy(args []string, move bool) error {
	verb := "cp"
	if move {
		verb = "mv"
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: shome fs %s SRC DST\n\n"+
			"  shome fs %s mini:model.bin gpu-box:\n"+
			"  shome fs %s mini:model.bin gpu-box:models/model.bin", verb, verb, verb)
	}
	src, err := parseRef(args[0])
	if err != nil {
		return err
	}
	dst, err := parseRef(args[1])
	if err != nil {
		return err
	}
	// A side with no machine is a file on the computer you are typing on.
	// That is the only way files enter or leave the cluster, so `cp` covers
	// it rather than making it a separate verb to learn.
	switch {
	case src.Node == "" && dst.Node == "":
		return fmt.Errorf("neither side names a machine, so this is a local copy -- use plain %s\n\n"+
			"To move a file into the cluster: shome fs %s %s mini:", verb, verb, args[0])
	case src.Node == "":
		return fsPut(args[0], dst, move)
	case dst.Node == "":
		return fsGet(src, args[1], move)
	}
	// `cp a:dir/file b:` means the same name at the top of b's storage, which
	// is what the shell idiom leads people to expect.
	if dst.Path == "" || strings.HasSuffix(args[1], "/") {
		dst.Path = strings.TrimSuffix(dst.Path, "/") + "/" + baseName(src.Path)
		dst.Path = strings.TrimPrefix(dst.Path, "/")
	}

	var t ctl.Transfer
	if err := call("POST", "/fs/transfer", map[string]any{
		"from_node": src.Node, "from_path": src.Path,
		"to_node": dst.Node, "to_path": dst.Path, "move": move,
	}, &t); err != nil {
		return err
	}
	fmt.Printf("%s %s -> %s (%s)\n", verb, src, dst, bytesShort(t.Size))
	return waitTransfer(t.ID, move)
}

// waitTransfer follows a transfer to its end.
//
// Worth waiting for rather than returning a handle: a copy between two
// machines takes seconds, and a command that returned immediately would leave
// people polling by hand to find out whether their file arrived.
// fsPut uploads a file from the computer the user is on into a machine's
// storage, relayed by the controller.
func fsPut(local string, dst ref, move bool) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		f.Close()
		return fsPutTree(local, dst, move)
	}
	if dst.Path == "" || strings.HasSuffix(dst.Path, "/") {
		dst.Path = strings.TrimSuffix(dst.Path, "/") + "/" + baseName(local)
		dst.Path = strings.TrimPrefix(dst.Path, "/")
	}

	fmt.Printf("up %s -> %s (%s)\n", local, dst, bytesShort(st.Size()))
	var t ctl.Transfer
	if err := callStream("POST", fmt.Sprintf("/fs/upload?node=%s&path=%s",
		url.QueryEscape(dst.Node), url.QueryEscape(dst.Path)), f, &t); err != nil {
		return err
	}
	if err := waitTransfer(t.ID, false); err != nil {
		return err
	}
	if move {
		// Only now, once the cluster has confirmed the write. Removing the
		// user's only copy before that would lose the file outright.
		if err := os.Remove(local); err != nil {
			return fmt.Errorf("uploaded, but could not remove %s: %w", local, err)
		}
		fmt.Printf("  the local copy of %s was removed\n", local)
	}
	return nil
}

// fsGet downloads one of the user's files from a machine to the computer they
// are on.
//
// The machine cannot be dialled, so this is two hops: ask it to send the file
// to the controller, wait, then stream it down.
func fsGet(src ref, local string, move bool) error {
	// A prefix with files under it and no file of exactly that name is a
	// directory. The index is the only thing that knows: the machine holding
	// the file cannot be dialled from here, so asking it would mean a round
	// trip through the controller to answer a question about a name.
	if files, err := fsIndex(src); err == nil && len(files) > 0 {
		exact := false
		for _, f := range files {
			if f.Path == src.Path {
				exact = true
				break
			}
		}
		if !exact {
			return fsGetTree(src, files, local, move)
		}
	}
	if local == "" || strings.HasSuffix(local, "/") || isDir(local) {
		local = strings.TrimSuffix(local, "/") + "/" + baseName(src.Path)
		local = strings.TrimPrefix(local, "/")
		if local == "" {
			local = baseName(src.Path)
		}
	}
	var t ctl.Transfer
	if err := call("POST", "/fs/fetch", map[string]any{
		"node": src.Node, "path": src.Path,
	}, &t); err != nil {
		return err
	}
	fmt.Printf("down %s -> %s (%s)\n", src, local, bytesShort(t.Size))
	if err := waitStaged(t.ID, src.Node); err != nil {
		return err
	}

	body, err := openStream(fmt.Sprintf("/fs/fetch/%d", t.ID))
	if err != nil {
		return err
	}
	defer body.Close()
	// Written beside the target and renamed, so an interrupted download
	// cannot be mistaken for a complete file.
	tmp := local + ".shome-part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, body)
	if err := out.Close(); err == nil && cerr == nil {
		cerr = os.Rename(tmp, local)
	}
	if cerr != nil {
		os.Remove(tmp)
		return cerr
	}
	fmt.Printf("  done\n")
	if move {
		if err := call("POST", "/fs/remove", map[string]any{
			"node": src.Node, "path": src.Path,
		}, nil); err != nil {
			return fmt.Errorf("downloaded, but could not remove %s: %w", src, err)
		}
		fmt.Printf("  the copy on %s was removed\n", src.Node)
	}
	return nil
}

// A directory is the unit people actually move: a dataset, a checkpoint
// directory, a source tree. The transfer API moves one file at a time, so
// walking the tree and moving each file is what "copy a directory" means
// here -- there is no archive step, and a copy interrupted halfway has
// transferred whole files rather than half of a tar.
//
// Symlinks are skipped rather than followed. Following them would copy a
// file from outside the tree, and there is no way to record the link itself,
// so the count is reported instead of silently dropping them. This is why a
// virtualenv does not survive being copied: its interpreter is a symlink.

// treeFile is one file in a tree, relative to the root of that tree.
type treeFile struct {
	Rel  string
	Size int64
}

// localTree lists the regular files under a local directory.
func localTree(dir string) (files []treeFile, total int64, links int, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			links++
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, treeFile{Rel: filepath.ToSlash(rel), Size: fi.Size()})
		total += fi.Size()
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, total, links, err
}

// treeDest is where a directory copy lands.
//
// `cp ./data mini:` and `cp ./data mini:backups/` both put the directory
// itself at that point, which is what cp -r does and therefore what people
// expect. Naming a path outright -- `cp ./data mini:other` -- renames it.
func treeDest(dir string, dst ref, arg string) ref {
	if dst.Path == "" || strings.HasSuffix(arg, "/") {
		dst.Path = joinPath(strings.TrimSuffix(dst.Path, "/"), baseName(strings.TrimSuffix(dir, "/")))
		dst.Path = strings.TrimPrefix(dst.Path, "/")
	}
	return dst
}

// fsPutTree uploads a local directory into a machine's storage.
func fsPutTree(dir string, dst ref, move bool) error {
	files, total, links, err := localTree(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s holds no files to copy", dir)
	}
	to := treeDest(dir, dst, dst.Path)
	fmt.Printf("up %s -> %s (%d file(s), %s)\n", dir, to, len(files), bytesShort(total))
	if links > 0 {
		fmt.Printf("  %d symlink(s) or special file(s) skipped\n", links)
	}
	for i, f := range files {
		fmt.Printf("[%d/%d] %s\n", i+1, len(files), f.Rel)
		src := filepath.Join(dir, filepath.FromSlash(f.Rel))
		if err := fsUploadOne(src, ref{Node: to.Node, Path: joinPath(to.Path, f.Rel)}); err != nil {
			return fmt.Errorf("after %d of %d file(s): %w", i, len(files), err)
		}
	}
	if move {
		// Only once every file is confirmed on the other side. Removing as
		// we went would leave a half-moved tree if one file failed.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("uploaded, but could not remove %s: %w", dir, err)
		}
		fmt.Printf("  the local copy of %s was removed\n", dir)
	}
	fmt.Printf("\n%s now holds %d file(s)\n", to, len(files))
	return nil
}

// fsUploadOne uploads a single file and waits for it, without the printing
// and the move handling that fsPut does for a lone file.
func fsUploadOne(local string, dst ref) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	var t ctl.Transfer
	if err := callStream("POST", fmt.Sprintf("/fs/upload?node=%s&path=%s",
		url.QueryEscape(dst.Node), url.QueryEscape(dst.Path)), f, &t); err != nil {
		return err
	}
	return waitTransfer(t.ID, false)
}

// fsGetTree downloads every file under a prefix on one machine.
func fsGetTree(src ref, files []ctl.FSEntry, local string, move bool) error {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	// Mirrors treeDest: a destination that is an existing directory, or ends
	// in a separator, gets the source directory inside it.
	if local == "" || local == "." || strings.HasSuffix(local, "/") || isDir(local) {
		local = filepath.Join(local, baseName(strings.TrimSuffix(src.Path, "/")))
	}
	fmt.Printf("down %s -> %s (%d file(s), %s)\n", src, local, len(files), bytesShort(total))
	for i, f := range files {
		rel := relTo(src.Path, f.Path)
		fmt.Printf("[%d/%d] %s\n", i+1, len(files), rel)
		out := filepath.Join(local, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		if err := fsDownloadOne(ref{Node: src.Node, Path: f.Path}, out); err != nil {
			return fmt.Errorf("after %d of %d file(s): %w", i, len(files), err)
		}
	}
	if move {
		if err := call("POST", "/fs/remove", map[string]any{
			"node": src.Node, "path": src.Path, "recursive": true,
		}, nil); err != nil {
			return fmt.Errorf("downloaded, but could not remove %s: %w", src, err)
		}
		fmt.Printf("  the copy on %s was removed\n", src.Node)
	}
	fmt.Printf("\n%s now holds %d file(s)\n", local, len(files))
	return nil
}

// fsDownloadOne fetches one file to an exact local path.
func fsDownloadOne(src ref, local string) error {
	var t ctl.Transfer
	if err := call("POST", "/fs/fetch", map[string]any{
		"node": src.Node, "path": src.Path,
	}, &t); err != nil {
		return err
	}
	if err := waitStaged(t.ID, src.Node); err != nil {
		return err
	}
	body, err := openStream(fmt.Sprintf("/fs/fetch/%d", t.ID))
	if err != nil {
		return err
	}
	defer body.Close()
	// Written beside the target and renamed, so an interrupted download
	// cannot be mistaken for a complete file.
	tmp := local + ".shome-part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, body)
	if err := out.Close(); err == nil && cerr == nil {
		cerr = os.Rename(tmp, local)
	}
	if cerr != nil {
		os.Remove(tmp)
		return cerr
	}
	return nil
}

// isDir reports whether a local path is an existing directory, so that
// `fs cp mini:f .` lands the file inside it.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// waitStaged waits for a machine to hand a fetched file to the controller.
func waitStaged(id int64, from string) error {
	deadline := time.Now().Add(10 * time.Minute)
	said := false
	for time.Now().Before(deadline) {
		var t ctl.Transfer
		if err := call("GET", fmt.Sprintf("/fs/transfer/%d", id), nil, &t); err != nil {
			return err
		}
		switch t.State {
		case ctl.TransferStaged:
			return nil
		case ctl.TransferFailed:
			return fmt.Errorf("transfer failed: %s", t.Error)
		}
		if !said {
			fmt.Printf("  waiting for %s to send it...\n", from)
			said = true
		}
		time.Sleep(700 * time.Millisecond)
	}
	return fmt.Errorf("%s has not sent the file; it may be asleep", from)
}

func waitTransfer(id int64, move bool) error {
	deadline := time.Now().Add(10 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		var t ctl.Transfer
		if err := call("GET", fmt.Sprintf("/fs/transfer/%d", id), nil, &t); err != nil {
			return err
		}
		if string(t.State) != last {
			switch t.State {
			case ctl.TransferSending:
				fmt.Printf("  reading from %s...\n", t.FromNode)
			case ctl.TransferStaged:
				fmt.Printf("  writing to %s...\n", t.ToNode)
			}
			last = string(t.State)
		}
		switch t.State {
		case ctl.TransferDone:
			if move {
				fmt.Printf("  done; the original on %s was removed\n", t.FromNode)
			} else {
				fmt.Printf("  done\n")
			}
			return nil
		case ctl.TransferFailed:
			return fmt.Errorf("transfer failed: %s", t.Error)
		}
		time.Sleep(700 * time.Millisecond)
	}
	return fmt.Errorf("transfer %d is still running; check with 'shome fs du'", id)
}

// fsSync makes one machine's copy match another's.
//
// One way, and additive: it copies what is missing or has changed size, and
// does not delete anything at the destination. Deleting to match would make a
// mistyped machine name destructive, and the space it would reclaim is
// something `fs rm` can do deliberately.
func fsSync(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: shome fs sync SRC: DST:\n\n" +
			"  shome fs sync mini:data gpu-box:data\n\n" +
			"Copies what is missing or has changed. Nothing is deleted at the\n" +
			"destination, so this only ever uses more space.")
	}
	src, err := parseRef(args[0])
	if err != nil {
		return err
	}
	dst, err := parseRef(args[1])
	if err != nil {
		return err
	}
	if src.Node == "" && dst.Node == "" {
		return fmt.Errorf("neither side names a machine, so this is a local copy -- use rsync")
	}
	if src.Node != "" && src.Node == dst.Node {
		return fmt.Errorf("source and destination are the same machine")
	}

	// One comparison for all three directions -- machine to machine, up from
	// here, down to here -- because "what is missing or has changed" is the
	// same question whichever side is local, and two implementations of it
	// would disagree about some edge of it eventually.
	srcFiles, err := syncSide(src)
	if err != nil {
		return err
	}
	if len(srcFiles) == 0 {
		return fmt.Errorf("nothing to sync: %s has no files", src)
	}
	dstFiles, err := syncSide(dst)
	if err != nil {
		return err
	}
	have := map[string]int64{}
	for _, f := range dstFiles {
		have[f.Rel] = f.Size
	}

	var todo []treeFile
	var bytes int64
	for _, f := range srcFiles {
		// Size only. Comparing content would mean hashing every file on both
		// machines on every sync, which for the common case -- a dataset that
		// grew -- costs far more than it catches.
		if sz, ok := have[f.Rel]; ok && sz == f.Size {
			continue
		}
		todo = append(todo, f)
		bytes += f.Size
	}
	if len(todo) == 0 {
		fmt.Printf("%s already matches %s (%d file(s))\n", dst, src, len(srcFiles))
		return nil
	}
	fmt.Printf("%d file(s), %s to copy from %s to %s\n",
		len(todo), bytesShort(bytes), src, dst)

	// Headroom is checked per transfer by the controller, so a sync that
	// would overrun the limit stops partway with a clear reason rather than
	// being refused wholesale -- the files already copied are still useful.
	for i, f := range todo {
		fmt.Printf("[%d/%d] %s\n", i+1, len(todo), f.Rel)
		if err := syncOne(src, dst, f.Rel); err != nil {
			return fmt.Errorf("after %d of %d file(s): %w", i, len(todo), err)
		}
	}
	fmt.Printf("\n%s now matches %s\n", dst, src)
	return nil
}

// syncSide lists one side of a sync as paths relative to its own root,
// whether that side is a machine or this computer.
//
// A local side that does not exist yet is empty rather than an error: that is
// the ordinary case for `sync mini:data ./data` the first time.
func syncSide(r ref) ([]treeFile, error) {
	if r.Node == "" {
		if !isDir(r.Local) {
			if _, err := os.Stat(r.Local); err == nil {
				return nil, fmt.Errorf("%s is a file, not a directory; use 'shome fs cp'", r.Local)
			}
			return nil, nil
		}
		files, _, links, err := localTree(r.Local)
		if err != nil {
			return nil, err
		}
		if links > 0 {
			fmt.Printf("note: %d symlink(s) or special file(s) in %s are skipped\n", links, r.Local)
		}
		return files, nil
	}
	entries, err := fsIndex(r)
	if err != nil {
		return nil, err
	}
	out := make([]treeFile, 0, len(entries))
	for _, e := range entries {
		out = append(out, treeFile{Rel: relTo(r.Path, e.Path), Size: e.Size})
	}
	return out, nil
}

// syncOne copies one relative path from one side of a sync to the other.
func syncOne(src, dst ref, rel string) error {
	switch {
	case src.Node == "":
		return fsUploadOne(filepath.Join(src.Local, filepath.FromSlash(rel)),
			ref{Node: dst.Node, Path: joinPath(dst.Path, rel)})
	case dst.Node == "":
		out := filepath.Join(dst.Local, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		return fsDownloadOne(ref{Node: src.Node, Path: joinPath(src.Path, rel)}, out)
	default:
		var t ctl.Transfer
		if err := call("POST", "/fs/transfer", map[string]any{
			"from_node": src.Node, "from_path": joinPath(src.Path, rel),
			"to_node": dst.Node, "to_path": joinPath(dst.Path, rel), "move": false,
		}, &t); err != nil {
			return err
		}
		return waitTransfer(t.ID, false)
	}
}

// fsIndex lists one machine's files under a prefix.
func fsIndex(r ref) ([]ctl.FSEntry, error) {
	q := "/fs?node=" + urlEscape(r.Node)
	if r.Path != "" {
		q += "&path=" + urlEscape(r.Path)
	}
	var out []ctl.FSEntry
	err := call("GET", q, nil, &out)
	return out, err
}

func fsRm(args []string) error {
	recursive := false
	var targets []string
	for _, a := range args {
		switch a {
		case "-r", "-R", "--recursive":
			recursive = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown option %q for 'shome fs rm'", a)
			}
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("usage: shome fs rm [NODE:]PATH [-r]\n\n" +
			"Without a machine, the path is removed from every machine that has it")
	}
	for _, target := range targets {
		r, err := parseRef(target)
		if err != nil {
			return err
		}
		nodes := []string{r.Node}
		if r.Node != "" {
			// Say when the index has no such file there. Not an error,
			// because the index lags a machine by up to a check-in and a
			// file created moments ago is genuinely removable -- but
			// reporting "removing" for a path that machine does not have
			// reads as success and is how a typo goes unnoticed.
			has, err := fsNodesWith(r.Path)
			if err == nil && !slices.Contains(has, r.Node) {
				fmt.Printf("note: %s is not in %s's listing.\n", r.Path, r.Node)
				if len(has) > 0 {
					fmt.Printf("      %s has it: shome fs rm %s:%s\n",
						strings.Join(has, ", "), has[0], r.Path)
				}
				fmt.Printf("      Asking anyway, in case it is newer than the listing.\n")
			}
		}
		if r.Node == "" {
			// A bare path means everywhere, which is the useful default for
			// reclaiming space -- but the machines are named as it goes, so
			// it is never a surprise what was removed.
			found, err := fsNodesWith(r.Path)
			if err != nil {
				return err
			}
			if len(found) == 0 {
				return fmt.Errorf("no machine has %q", r.Path)
			}
			nodes = found
		}
		for _, n := range nodes {
			if err := call("POST", "/fs/remove", map[string]any{
				"node": n, "path": r.Path, "recursive": recursive,
			}, nil); err != nil {
				return fmt.Errorf("%s:%s: %w", n, r.Path, err)
			}
			fmt.Printf("removing %s:%s\n", n, r.Path)
		}
	}
	fmt.Printf("\nQueued. The machines apply it on their next check-in;\n")
	fmt.Printf("'shome fs du' will show the space back.\n")
	return nil
}

// fsNodesWith finds which machines hold a path.
func fsNodesWith(path string) ([]string, error) {
	var entries []ctl.FSEntry
	if err := call("GET", "/fs?path="+urlEscape(path), nil, &entries); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if !seen[e.Node] {
			seen[e.Node] = true
			out = append(out, e.Node)
		}
	}
	sort.Strings(out)
	return out, nil
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// relTo strips a prefix from an indexed path, so a sync can rebuild it under
// a different prefix on the destination.
func relTo(prefix, path string) string {
	if prefix == "" {
		return path
	}
	return strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")
}

func joinPath(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return strings.TrimSuffix(prefix, "/") + "/" + rel
}

func mibShort(m int64) string {
	if m < 0 {
		return "-"
	}
	return bytesShort(m << 20)
}
