package folderwatchtrigger

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TIBCOSoftware/flogo-lib/core/action"
	"github.com/TIBCOSoftware/flogo-lib/core/trigger"
)

//...................................................................................
var (
	// ErrDurationTooShort occurs when calling the watcher's Start
	// method with a duration that's less than 1 nanosecond.
	ErrDurationTooShort = errors.New("error: duration is less than 1ns")

	// ErrWatcherRunning occurs when trying to call the watcher's
	// Start method and the polling cycle is still already running
	// from previously calling Start and not yet calling Close.
	ErrWatcherRunning = errors.New("error: watcher is already running")

	// ErrWatchedFileDeleted is an error that occurs when a file or folder that was
	// being watched has been deleted.
	ErrWatchedFileDeleted = errors.New("error: watched file or folder deleted")
)

// An Op is a type that is used to describe what type
// of event has occurred during the watching process.
type Op uint32

// Ops
const (
	Create Op = iota
	Write
	Remove
	Rename
	Chmod
	Move
)

var ops = map[Op]string{
	Create: "CREATE",
	Write:  "WRITE",
	Remove: "REMOVE",
	Rename: "RENAME",
	Chmod:  "CHMOD",
	Move:   "MOVE",
}

// String prints the string version of the Op consts
func (e Op) String() string {
	if op, found := ops[e]; found {
		return op
	}
	return "???"
}

// An Event describes an event that is received when files or directory
// changes occur. It includes the os.FileInfo of the changed file or
// directory and the type of event that's occurred and the full path of the file.
type Event struct {
	Op
	Path string
	os.FileInfo
}

// String returns a string depending on what type of event occurred and the
// file name associated with the event.
func (e Event) String() string {
	if e.FileInfo != nil {
		pathType := "FILE"
		if e.IsDir() {
			pathType = "DIRECTORY"
		}

		fmt.Println(pathType)
		fmt.Println(e.Name())
		fmt.Println(e.Op)

		//......................................................................

		//fmt.Println("%s %q %s", pathType, e.Name(), e.Op)

		//Replacing the follwing line of code
		//return fmt.Sprintf("%s %q %s [%s]", pathType, e.Name(), e.Op, e.Path)

		return fmt.Sprintf("%s", e.Path)

	}
	return "???"
}
//Watcher has all properties for EventWatcher
type Watcher struct {
	Event  chan Event
	Error  chan error
	Closed chan struct{}
	close  chan struct{}
	wg     *sync.WaitGroup

	// mu protects the following.
	mu           *sync.Mutex
	running      bool
	names        map[string]bool        // bool for recursive or not.
	files        map[string]os.FileInfo // map of files.
	ignored      map[string]struct{}    // ignored files or directories.
	ops          map[Op]struct{}        // Op filtering.
	ignoreHidden bool                   // ignore hidden files or not.
	maxEvents    int                    // max sent events per cycle
}

// New creates a new Watcher.
func New() *Watcher {
	// Set up the WaitGroup for w.Wait().
	var wg sync.WaitGroup
	wg.Add(1)

	return &Watcher{
		Event:   make(chan Event),
		Error:   make(chan error),
		Closed:  make(chan struct{}),
		close:   make(chan struct{}),
		mu:      new(sync.Mutex),
		wg:      &wg,
		files:   make(map[string]os.FileInfo),
		ignored: make(map[string]struct{}),
		names:   make(map[string]bool),
	}
}

// SetMaxEvents controls the maximum amount of events that are sent on--
// the Event channel per watching cycle. If max events is less than 1, there is
// no limit, which is the default.
func (w *Watcher) SetMaxEvents(delta int) {
	w.mu.Lock()
	w.maxEvents = delta
	w.mu.Unlock()
}

// IgnoreHiddenFiles sets the watcher to ignore any file or directory
// that starts with a dot.
func (w *Watcher) IgnoreHiddenFiles(ignore bool) {
	w.mu.Lock()
	w.ignoreHidden = ignore
	w.mu.Unlock()
}

// FilterOps filters which event op types should be returned--
// when an event occurs.
func (w *Watcher) FilterOps(ops ...Op) {
	w.mu.Lock()
	w.ops = make(map[Op]struct{})
	for _, op := range ops {
		w.ops[op] = struct{}{}
	}
	w.mu.Unlock()
}

// Add adds either a single file or directory to the file list.
func (w *Watcher) Add(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// If name is on the ignored list or if hidden files are
	// ignored and name is a hidden file or directory, simply return.
	_, ignored := w.ignored[name]
	if ignored || (w.ignoreHidden && strings.HasPrefix(name, ".")) {
		return nil
	}

	// Add the directory's contents to the files list.
	fileList, err := w.list(name)
	if err != nil {
		return err
	}
	for k, v := range fileList {
		w.files[k] = v
	}

	// Add the name to the names list.
	w.names[name] = false

	return nil
}

func (w *Watcher) list(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	// Make sure name exists.
	stat, err := os.Stat(name)
	if err != nil {
		return nil, err
	}

	fileList[name] = stat

	// If it's not a directory, just return.
	if !stat.IsDir() {
		return fileList, nil
	}

	// It's a directory.
	fInfoList, err := ioutil.ReadDir(name)
	if err != nil {
		return nil, err
	}
	// Add all of the files in the directory to the file list as long
	// as they aren't on the ignored list or are hidden files if ignoreHidden
	// is set to true.
	for _, fInfo := range fInfoList {
		path := filepath.Join(name, fInfo.Name())
		_, ignored := w.ignored[path]
		if ignored || (w.ignoreHidden && strings.HasPrefix(fInfo.Name(), ".")) {
			continue
		}
		fileList[path] = fInfo
	}
	return fileList, nil
}

// AddRecursive adds either a single file or directory recursively to the file list.
func (w *Watcher) AddRecursive(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	fileList, err := w.listRecursive(name)
	if err != nil {
		return err
	}
	for k, v := range fileList {
		w.files[k] = v
	}

	// Add the name to the names list.
	w.names[name] = true

	return nil
}

func (w *Watcher) listRecursive(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	return fileList, filepath.Walk(name, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// If path is ignored and it's a directory, skip the directory. If it's
		// ignored and it's a single file, skip the file.
		_, ignored := w.ignored[path]
		if ignored || (w.ignoreHidden && strings.HasPrefix(info.Name(), ".")) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Add the path and it's info to the file list.
		fileList[path] = info
		return nil
	})
}

// Remove removes either a single file or directory from the file's list.
func (w *Watcher) Remove(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// Remove the name from w's names list.
	delete(w.names, name)

	// If name is a single file, remove it and return.
	info, found := w.files[name]
	if !found {
		return nil // Doesn't exist, just return.
	}
	if !info.IsDir() {
		delete(w.files, name)
		return nil
	}

	// Delete the actual directory from w.files
	delete(w.files, name)

	// If it's a directory, delete all of it's contents from w.files.
	for path := range w.files {
		if filepath.Dir(path) == name {
			delete(w.files, path)
		}
	}
	return nil
}

// RemoveRecursive Remove removes either a single file or a directory recursively from
// the file's list.
func (w *Watcher) RemoveRecursive(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// Remove the name from w's names list.
	delete(w.names, name)

	// If name is a single file, remove it and return.
	info, found := w.files[name]
	if !found {
		return nil // Doesn't exist, just return.
	}
	if !info.IsDir() {
		delete(w.files, name)
		return nil
	}

	// If it's a directory, delete all of it's contents recursively
	// from w.files.
	for path := range w.files {
		if strings.HasPrefix(path, name) {
			delete(w.files, path)
		}
	}
	return nil
}

// Ignore adds paths that should be ignored.
//
// For files that are already added, Ignore removes them.
func (w *Watcher) Ignore(paths ...string) (err error) {
	for _, path := range paths {
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		// Remove any of the paths that were already added.
		if err := w.RemoveRecursive(path); err != nil {
			return err
		}
		w.mu.Lock()
		w.ignored[path] = struct{}{}
		w.mu.Unlock()
	}
	return nil
}
//WatchedFiles prepares a map of WatchedFiles
func (w *Watcher) WatchedFiles() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.files
}

// fileInfo is an implementation of os.FileInfo that can be used
// as a mocked os.FileInfo when triggering an event when the specified
// os.FileInfo is nil.
type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	sys     interface{}
	dir     bool
}

func (fs *fileInfo) IsDir() bool {
	return fs.dir
}
func (fs *fileInfo) ModTime() time.Time {
	return fs.modTime
}
func (fs *fileInfo) Mode() os.FileMode {
	return fs.mode
}
func (fs *fileInfo) Name() string {
	return fs.name
}
func (fs *fileInfo) Size() int64 {
	return fs.size
}
func (fs *fileInfo) Sys() interface{} {
	return fs.sys
}

// TriggerEvent is a method that can be used to trigger an event, separate to
// the file watching process.
func (w *Watcher) TriggerEvent(eventType Op, file os.FileInfo) {
	w.Wait()
	if file == nil {
		file = &fileInfo{name: "triggered event", modTime: time.Now()}
	}
	w.Event <- Event{Op: eventType, Path: "-", FileInfo: file}
}

func (w *Watcher) retrieveFileList() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	fileList := make(map[string]os.FileInfo)

	var list map[string]os.FileInfo
	var err error

	for name, recursive := range w.names {
		if recursive {
			list, err = w.listRecursive(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.Error <- ErrWatchedFileDeleted
					w.mu.Unlock()
					w.RemoveRecursive(name)
					w.mu.Lock()
				} else {
					w.Error <- err
				}
			}
		} else {
			list, err = w.list(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.Error <- ErrWatchedFileDeleted
					w.mu.Unlock()
					w.Remove(name)
					w.mu.Lock()
				} else {
					w.Error <- err
				}
			}
		}
		// Add the file's to the file list.
		for k, v := range list {
			fileList[k] = v
		}
	}

	return fileList
}

// Start begins the polling cycle which repeats every specified
// duration until Close is called.
func (w *Watcher) Start(d time.Duration) error {
	// Return an error if d is less than 1 nanosecond.
	if d < time.Nanosecond {
		return ErrDurationTooShort
	}

	// Make sure the Watcher is not already running.
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return ErrWatcherRunning
	}
	w.running = true
	w.mu.Unlock()

	// Unblock w.Wait().
	w.wg.Done()

	for {
		// done lets the inner polling cycle loop know when the
		// current cycle's method has finished executing.
		done := make(chan struct{})

		// Any events that are found are first piped to evt before
		// being sent to the main Event channel.
		evt := make(chan Event)

		// Retrieve the file list for all watched file's and dirs.
		fileList := w.retrieveFileList()

		// cancel can be used to cancel the current event polling function.
		cancel := make(chan struct{})

		// Look for events.
		go func() {
			w.pollEvents(fileList, evt, cancel)
			done <- struct{}{}
		}()

		// numEvents holds the number of events for the current cycle.
		numEvents := 0

	inner:
		for {
			select {
			case <-w.close:
				close(cancel)
				close(w.Closed)
				return nil
			case event := <-evt:
				if len(w.ops) > 0 { // Filter Ops.
					_, found := w.ops[event.Op]
					if !found {
						continue
					}
				}
				numEvents++
				if w.maxEvents > 0 && numEvents > w.maxEvents {
					close(cancel)
					break inner
				}
				w.Event <- event
			case <-done: // Current cycle is finished.
				break inner
			}
		}

		// Update the file's list.
		w.mu.Lock()
		w.files = fileList
		w.mu.Unlock()

		// Sleep and then continue to the next loop iteration.
		time.Sleep(d)
	}
}
//SameFile looks for changes in samefile
func SameFile(fi1, fi2 os.FileInfo) bool {
	return os.SameFile(fi1, fi2)
}

func (w *Watcher) pollEvents(files map[string]os.FileInfo, evt chan Event,
	cancel chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Store create and remove events for use to check for rename events.
	creates := make(map[string]os.FileInfo)
	removes := make(map[string]os.FileInfo)

	// Check for removed files.
	for path, info := range w.files {
		if _, found := files[path]; !found {
			removes[path] = info
		}
	}

	// Check for created files, writes and chmods.
	for path, info := range files {
		oldInfo, found := w.files[path]
		if !found {
			// A file was created.
			creates[path] = info
			continue
		}
		if oldInfo.ModTime() != info.ModTime() {
			select {
			case <-cancel:
				return
			case evt <- Event{Write, path, info}:
			}
		}
		if oldInfo.Mode() != info.Mode() {
			select {
			case <-cancel:
				return
			case evt <- Event{Chmod, path, info}:
			}
		}
	}

	// Check for renames and moves.
	for path1, info1 := range removes {
		for path2, info2 := range creates {
			if SameFile(info1, info2) {
				e := Event{
					Op:       Move,
					Path:     fmt.Sprintf("%s -> %s", path1, path2),
					FileInfo: info1,
				}
				// If they are from the same directory, it's a rename
				// instead of a move event.
				if filepath.Dir(path1) == filepath.Dir(path2) {
					e.Op = Rename
				}

				delete(removes, path1)
				delete(creates, path2)

				select {
				case <-cancel:
					return
				case evt <- e:
				}
			}
		}
	}

	// Send all the remaining create and remove events.
	for path, info := range creates {
		select {
		case <-cancel:
			return
		case evt <- Event{Create, path, info}:
		}
	}
	for path, info := range removes {
		select {
		case <-cancel:
			return
		case evt <- Event{Remove, path, info}:
		}
	}
}

// Wait blocks until the watcher is started.---
func (w *Watcher) Wait() {
	w.wg.Wait()
}
//Close sends a close signal to start method
func (w *Watcher) Close() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	w.files = make(map[string]os.FileInfo)
	w.names = make(map[string]bool)
	w.mu.Unlock()
	// Send a close signal to the Start method.
	w.close <- struct{}{}
}

//............................................................................................................

// MyTriggerFactory My Trigger factory
type MyTriggerFactory struct {
	metadata *trigger.Metadata
}

//NewFactory create a new Trigger factory
func NewFactory(md *trigger.Metadata) trigger.Factory {
	return &MyTriggerFactory{metadata: md}
}

//New Creates a new trigger instance for a given id
func (t *MyTriggerFactory) New(config *trigger.Config) trigger.Trigger {
	return &MyTrigger{metadata: t.metadata, config: config}
}

// MyTrigger is a stub for your Trigger implementation
type MyTrigger struct {
	metadata *trigger.Metadata
	runner   action.Runner
	config   *trigger.Config
}

// Init implements trigger.Trigger.Init
func (t *MyTrigger) Init(runner action.Runner) {
	t.runner = runner
}

// Metadata implements trigger.Trigger.Metadata
func (t *MyTrigger) Metadata() *trigger.Metadata {
	return t.metadata
}

// Start implements trigger.Trigger.Start
func (t *MyTrigger) Start() error {
	// start the trigger

	//adding the folder watcher code here...........

	watchFolder()

	//..............................................
	return nil
}

//New function created with code in main method folderwatcher
func watchFolder() {
	w := New()

	// SetMaxEvents to 1 to allow at most 1 event's to be received
	// on the Event channel per watching cycle.
	//
	// If SetMaxEvents is not set, the default is to send all events.
	w.SetMaxEvents(1)

	// Only notify rename and move events.
	w.FilterOps(Rename, Move, Create, Remove)

	go func() {
		for {
			select {
			case event := <-w.Event:
				fmt.Println(event) // Print the event's info.
			case err := <-w.Error:
				log.Fatalln(err)
			case <-w.Closed:
				return
			}
		}
	}()

	// Watch this folder for changes.
	if err := w.Add("d:folderwatchdemo"); err != nil {
		log.Fatalln(err)
	}

	// Watch test_folder recursively for changes.
	if err := w.AddRecursive("d:folderwatchdemo"); err != nil {
		log.Fatalln(err)
	}

	// Print a list of all of the files and folders currently
	// being watched and their paths.
	for path, f := range w.WatchedFiles() {
		fmt.Printf("%s: %s\n", path, f.Name())
	}

	fmt.Println()

	// Trigger 2 events after watcher started.
	go func() {
		w.Wait()
		w.TriggerEvent(Create, nil)
		w.TriggerEvent(Remove, nil)
	}()

	// Start the watching process - it'll check for changes every 100ms.
	if err := w.Start(time.Millisecond * 100); err != nil {
		log.Fatalln(err)
	}
}

// Stop implements trigger.Trigger.Start
func (t *MyTrigger) Stop() error {
	// stop the trigger
	return nil
}
