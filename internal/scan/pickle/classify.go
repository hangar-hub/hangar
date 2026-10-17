package pickle

import (
	"slices"
	"strings"
)

// importMapping and nameMapping are CPython's Lib/_compat_pickle.py
// IMPORT_MAPPING and NAME_MAPPING (Python 3.11): the Python 2 names that
// an unpickler's fix_imports maps to Python 3 ones for pickles of protocol
// 0 to 2. A pickle can name commands.getoutput and get
// subprocess.getoutput, so each import is classified under both names.
var importMapping = map[string]string{
	"__builtin__": "builtins", "copy_reg": "copyreg", "Queue": "queue", "SocketServer": "socketserver",
	"ConfigParser": "configparser", "repr": "reprlib", "tkFileDialog": "tkinter.filedialog",
	"tkSimpleDialog": "tkinter.simpledialog", "tkColorChooser": "tkinter.colorchooser",
	"tkCommonDialog": "tkinter.commondialog", "Dialog": "tkinter.dialog", "Tkdnd": "tkinter.dnd",
	"tkFont": "tkinter.font", "tkMessageBox": "tkinter.messagebox", "ScrolledText": "tkinter.scrolledtext",
	"Tkconstants": "tkinter.constants", "Tix": "tkinter.tix", "ttk": "tkinter.ttk", "Tkinter": "tkinter",
	"markupbase": "_markupbase", "_winreg": "winreg", "thread": "_thread", "dummy_thread": "_dummy_thread",
	"dbhash": "dbm.bsd", "dumbdbm": "dbm.dumb", "dbm": "dbm.ndbm", "gdbm": "dbm.gnu", "xmlrpclib": "xmlrpc.client",
	"SimpleXMLRPCServer": "xmlrpc.server", "httplib": "http.client", "htmlentitydefs": "html.entities",
	"HTMLParser": "html.parser", "Cookie": "http.cookies", "cookielib": "http.cookiejar",
	"BaseHTTPServer": "http.server", "test.test_support": "test.support", "commands": "subprocess",
	"urlparse": "urllib.parse", "robotparser": "urllib.robotparser", "urllib2": "urllib.request",
	"anydbm": "dbm", "_abcoll": "collections.abc", "cPickle": "pickle", "_elementtree": "xml.etree.ElementTree",
	"FileDialog": "tkinter.filedialog", "SimpleDialog": "tkinter.simpledialog", "DocXMLRPCServer": "xmlrpc.server",
	"SimpleHTTPServer": "http.server", "CGIHTTPServer": "http.server", "UserDict": "collections",
	"UserList": "collections", "UserString": "collections", "whichdb": "dbm", "StringIO": "io", "cStringIO": "io",
}

// nameMapping holds the NAME_MAPPING entries that change the module to
// one other than importMapping gives; the rest map exceptions and
// builtins to builtins.
var nameMapping = map[[2]string][2]string{
	{"__builtin__", "reduce"}:                  {"functools", "reduce"},
	{"__builtin__", "intern"}:                  {"sys", "intern"},
	{"itertools", "izip"}:                      {"builtins", "zip"},
	{"itertools", "imap"}:                      {"builtins", "map"},
	{"itertools", "ifilter"}:                   {"builtins", "filter"},
	{"_socket", "fromfd"}:                      {"socket", "fromfd"},
	{"_multiprocessing", "Connection"}:         {"multiprocessing.connection", "Connection"},
	{"multiprocessing.process", "Process"}:     {"multiprocessing.context", "Process"},
	{"multiprocessing.forking", "Popen"}:       {"multiprocessing.popen_fork", "Popen"},
	{"urllib", "getproxies"}:                   {"urllib.request", "getproxies"},
	{"urllib", "urlopen"}:                      {"urllib.request", "urlopen"},
	{"urllib", "urlretrieve"}:                  {"urllib.request", "urlretrieve"},
	{"urllib", "urlcleanup"}:                   {"urllib.request", "urlcleanup"},
	{"urllib", "pathname2url"}:                 {"urllib.request", "pathname2url"},
	{"urllib", "url2pathname"}:                 {"urllib.request", "url2pathname"},
	{"socket", "_socketobject"}:                {"socket", "SocketType"},
	{"multiprocessing", "AuthenticationError"}: {"multiprocessing.context", "AuthenticationError"},
}

// normaliseModule maps Python 2 module names to their Python 3 names, as
// pickle's fix_imports does.
func normaliseModule(m string) string {
	switch m {
	case "__builtins__", "exceptions":
		return "builtins"
	}
	if to, ok := importMapping[m]; ok {
		return to
	}
	return m
}

// compatName returns the global an unpickler imports for module and name
// with fix_imports.
func compatName(module, name string) (string, string) {
	if to, ok := nameMapping[[2]string{module, name}]; ok {
		return to[0], to[1]
	}
	return normaliseModule(module), name
}

func denied(path string) bool {
	for _, e := range unsafeGlobals {
		if prefix, ok := strings.CutSuffix(e, ".*"); ok {
			if path == prefix || strings.HasPrefix(path, prefix+".") {
				return true
			}
			continue
		}
		if path == e || strings.HasPrefix(path, e+".") {
			return true
		}
	}
	return false
}

func isDunder(s string) bool {
	return len(s) > 4 && strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__")
}

// classifyOne classifies a global under one name.
func classifyOne(module, name string) Classification {
	if denied(module + "." + name) {
		return Unsafe
	}
	if strings.Contains(name, ".") {
		// Protocol 4 resolves a dotted name attribute by attribute, so it
		// can walk from a harmless module to a dangerous one, as
		// collections.abc.sys.modules does, or into a function's globals.
		parts := strings.Split(name, ".")
		for i, p := range parts {
			if isDunder(p) || p == "" {
				return Unsafe
			}
			if denied(strings.Join(parts[i:], ".")) {
				return Unsafe
			}
		}
		return Suspicious
	}
	if slices.Contains(safeGlobals, module+"."+name) {
		return Safe
	}
	for _, s := range safeSuffixes {
		if module == s.module && strings.HasSuffix(name, s.suffix) {
			return Safe
		}
	}
	return Suspicious
}

// Classify returns the classification of a global by the lists in
// lists.go, under the name the pickle gives and the name an unpickler's
// fix_imports maps it to. Partial wrappers are Suspicious here; Scan
// raises them to Unsafe when the same pickle imports an unsafe target.
func Classify(module, name string) Classification {
	m, n := compatName(module, name)
	if m == module && n == name {
		return classifyOne(module, name)
	}
	mapped := classifyOne(m, n)
	if py2Only(module) {
		// Python 3 has no module of the old name, so only the mapped
		// import can run.
		if denied(module + "." + name) {
			return Unsafe
		}
		return mapped
	}
	if raw := classifyOne(module, name); rank(raw) > rank(mapped) {
		return raw
	}
	return mapped
}

// py2Only reports whether a module name exists only in Python 2.
func py2Only(module string) bool {
	if module == "__builtins__" || module == "exceptions" {
		return true
	}
	_, ok := importMapping[module]
	return ok && module != "dbm"
}

func isPartial(module, name string) bool {
	m, n := compatName(module, name)
	return slices.Contains(partialGlobals, m+"."+n)
}
