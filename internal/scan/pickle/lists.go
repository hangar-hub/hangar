package pickle

// The allow and deny lists for pickle imports (META-2). Each entry is a
// `module.name` global, or a `module.*` prefix that matches the module and
// every submodule. Sources, checked on 2026-10-13:
//
//   - [picklescan] github.com/mmaitre314/picklescan 1.0.5,
//     src/picklescan/scanner.py, _safe_globals and _unsafe_globals.
//   - [torch] PyTorch 2.9.0, torch/_weights_only_unpickler.py,
//     _get_allowed_globals and _tensor_rebuild_functions: what torch.load
//     accepts with weights_only=True, its default since 2.6.
//   - [hf] Hugging Face Hub documentation, "Pickle Scanning"
//     (https://huggingface.co/docs/hub/security-pickle).
//   - [spec] docs/spec/07-indexing-and-scanning.md.
//
// docs/research/pickle-scanning.md explains the choices.

// safeGlobals are tensor rebuild and container functions that cannot run
// arbitrary code.
var safeGlobals = []string{
	"collections.OrderedDict",                             // [picklescan] [torch] [spec]
	"collections.Counter",                                 // [torch]
	"torch._utils._rebuild_tensor",                        // [torch]
	"torch._utils._rebuild_tensor_v2",                     // [picklescan] [torch] [spec]
	"torch._utils._rebuild_tensor_v3",                     // [torch]
	"torch._utils._rebuild_parameter",                     // [torch] [spec]
	"torch._utils._rebuild_parameter_with_state",          // [torch]
	"torch._utils._rebuild_qtensor",                       // [torch]
	"torch._utils._rebuild_sparse_tensor",                 // [torch]
	"torch._utils._rebuild_meta_tensor_no_storage",        // [torch]
	"torch._utils._rebuild_nested_tensor",                 // [torch]
	"torch._utils._rebuild_wrapper_subclass",              // [torch]
	"torch._utils._rebuild_device_tensor_from_cpu_tensor", // [torch]
	"torch._utils._rebuild_device_tensor_from_numpy",      // [torch]
	"torch._tensor._rebuild_from_type_v2",                 // [torch]
	"torch.serialization._get_layout",                     // [torch]
	"torch.nn.parameter.Parameter",                        // [torch]
	"torch.Size",                                          // [torch]
	"torch.Tensor",                                        // [torch]
	"torch.device",                                        // [torch]
	"torch.float16", "torch.float32", "torch.float64",     // [torch] dtypes
	"torch.bfloat16",
	"torch.int8", "torch.int16", "torch.int32", "torch.int64", "torch.uint8",
	"torch.bool", "torch.complex64", "torch.complex128",
	"torch.per_tensor_affine", "torch.per_channel_affine", // [torch] quantisation schemes
	"torch.per_tensor_symmetric", "torch.per_channel_symmetric", "torch.per_channel_affine_float_qparams",
	"numpy.core.multiarray._reconstruct",  // [picklescan] [spec]
	"numpy._core.multiarray._reconstruct", // [picklescan]
	"numpy.core.multiarray.scalar",        // NumPy scalars; constructs a value from a dtype and bytes
	"numpy._core.multiarray.scalar",       // the same in NumPy 2
	"numpy.dtype",                         // [picklescan] [spec]
	"numpy.ndarray",                       // [picklescan] [spec]
	"builtins.set",                        // [torch] [spec]
	"builtins.bytearray",                  // [torch]
	"builtins.complex",                    // [torch]
	"_codecs.encode",                      // [torch] [spec]
}

// safeSuffixes are module and name-suffix pairs: torch storage classes such
// as torch.FloatStorage ([picklescan] [torch] [spec] "torch.*Storage"),
// and NumPy dtype classes such as numpy.dtypes.Float32DType.
var safeSuffixes = []struct{ module, suffix string }{
	{"torch", "Storage"},
	{"numpy.dtypes", "DType"},
}

// unsafeGlobals can run code, touch the file system or the network, or
// load more pickles. Prefix entries end in `.*`.
var unsafeGlobals = []string{
	// [picklescan] [spec]; __import__, execfile and input are added for
	// the same reason.
	"builtins.eval", "builtins.exec", "builtins.compile", "builtins.getattr", "builtins.apply",
	"builtins.open", "builtins.breakpoint", "builtins.__import__", "builtins.execfile", "builtins.input",
	// [picklescan]
	"cloudpickle.cloudpickle._builtin_type", "cloudpickle.cloudpickle._make_function",
	"cloudpickle.cloudpickle._function_setstate", "cloudpickle.cloudpickle._make_cell",
	"cloudpickle.cloudpickle._make_empty_cell", "cloudpickle.cloudpickle.subimport",
	"types.CodeType", "aiohttp.*", "asyncio.*", "bdb.*", "commands.*", "ctypes.*", "httplib.*",
	"logging.FileHandler", "_io.FileIO", "io.FileIO", "numpy.f2py.*", "numpy.testing._private.utils.*",
	"nt.*", "posix.*", "os.*", "requests.*", "runpy.*", "shutil.*", "socket.*", "ssl.*", "subprocess.*", "sys.*",
	"_operator.attrgetter", "_operator.itemgetter", "_operator.methodcaller",
	"operator.attrgetter", "operator.itemgetter", "operator.methodcaller",
	"code.*", "cProfile.*", "distutils.file_util.*", "doctest.debug_script", "ensurepip._run_pip",
	"idlelib.*", "imaplib.IMAP4_stream", "lib2to3.pgen2.*", "pdb.*", "pickle.*", "_pickle.*", "pip.*",
	"pkgutil.resolve_name", "pty.*", "profile.*", "pydoc.*", "test.*", "timeit.*",
	"torch._dynamo.guards.GuardBuilder.get", "torch._inductor.codecache.compile_file",
	"torch.fx.experimental.symbolic_shapes.ShapeEnv.evaluate_guards_expression",
	"torch.jit.unsupported_tensor_ops.execWrapper", "torch.serialization.load",
	"torch.utils._config_module.*", "torch.utils.bottleneck.*", "torch.utils.collect_env.run",
	"torch.utils.data.datapipes.utils.decoder.basichandlers",
	"trace.*", "urllib.*", "uuid.*", "venv.*", "webbrowser.*", "_osx_support.*", "_aix_support.*", "_pyrepl.*",
	// [hangar] Found in the phase 3 review: each runs code, loads a
	// native library or another pickle, writes files, or reaches the
	// import system, and none is in picklescan's list.
	"_posixsubprocess.*", "_winapi.*", "_ctypes.*", "zipimport.*", "_imp.*", "imp.*",
	"_frozen_importlib.*", "_frozen_importlib_external.*",
	"builtins.setattr", "builtins.delattr", "builtins.globals", "builtins.locals", "builtins.vars",
	"_io.open", "io.open", "_io.open_code", "io.open_code", "codecs.open", "shelve.*", "dill.*", "cloudpickle.*",
	"torch.utils.cpp_extension.*", "torch.ops.load_library", "torch._ops.*", "torch.classes.load_library",
	"numpy.load", "numpy.lib.npyio.load", "numpy.ctypeslib.*", "numpy.distutils.*",
	"joblib.load", "joblib.numpy_pickle.load", "pandas.read_pickle", "pandas.io.pickle.*", "antigravity.*",
	// [spec]
	"importlib.*", "http.client.*", "torch.load", "torch.hub.*", "multiprocessing.*", "marshal.*",
}

// partialGlobals wrap another callable. [spec]: unsafe when the pickle
// also imports an unsafe target, suspicious otherwise. ([picklescan]
// treats functools.partial as unsafe outright.)
var partialGlobals = []string{"functools.partial", "_functools.partial"}
