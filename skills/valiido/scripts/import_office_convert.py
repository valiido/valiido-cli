"""Run a local LibreOffice format conversion in a private, macro-disabled profile."""
import ctypes
import os
from pathlib import Path
import shutil
import subprocess
import sys
from tempfile import TemporaryDirectory


def export(source, output):
    binary = shutil.which("soffice")
    if not binary:
        raise ValueError("LibreOffice is required")
    folder = Path(binary).resolve().parent
    with TemporaryDirectory() as work:
        staged = Path(work) / ("input" + source.suffix.lower())
        shutil.copyfile(source, staged)
        source = staged
        profile = Path(work) / "profile"
        user = profile / "user"
        user.mkdir(parents=True)
        (user / "registrymodifications.xcu").write_text(
            '<oor:items xmlns:oor="http://openoffice.org/2001/registry">'
            '<item oor:path="/org.openoffice.Office.Common/Security/Scripting">'
            '<prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop>'
            '</item><item oor:path="/org.openoffice.Office.Paths/Paths">'
            '<node oor:name="Temp" oor:op="fuse"><prop oor:name="WritePath" oor:op="fuse">'
            f'<value>{Path(work).as_uri()}</value></prop></node></item></oor:items>', encoding="utf-8")
        # LibreOfficeKit has no GUI or IPC service. Run the native parser in a
        # short-lived worker so its libraries and profile never affect Python.
        library = next((folder / n for n in ("libsofficeapp.so", "libmergedlo.so")
                        if (folder / n).is_file()), None)
        env = dict(os.environ, SAL_USE_VCLPLUGIN="svp")
        if library:
            env["LD_LIBRARY_PATH"] = str(folder)
            env["URE_BOOTSTRAP"] = "vnd.sun.star.pathname:" + str(folder / "fundamentalrc")
            command = [sys.executable, "-I", __file__, library, profile, source.resolve(), output.resolve()]
        else:
            command = [binary, "-env:UserInstallation=" + profile.as_uri(), "--headless",
                       "--convert-to", output.suffix[1:], "--outdir", work, source.resolve()]
        subprocess.run(command, check=True, cwd=work, env=env, timeout=90,
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if not library:
            shutil.move(Path(work) / (source.stem + output.suffix), output)
        if not output.is_file():
            raise ValueError("Office export failed")


def kit_export(library, profile, source, output):
    # Stable leading members of LibreOfficeKit(Class) and LibreOfficeKitDocument(Class).
    ptr, string = ctypes.c_void_p, ctypes.c_char_p
    destroy = ctypes.CFUNCTYPE(None, ptr)
    load = ctypes.CFUNCTYPE(ptr, ptr, string)
    save = ctypes.CFUNCTYPE(ctypes.c_int, ptr, string, string, string)

    class KitClass(ctypes.Structure):
        _fields_ = [("size", ctypes.c_size_t), ("destroy", destroy), ("documentLoad", load)]

    class DocumentClass(ctypes.Structure):
        _fields_ = [("size", ctypes.c_size_t), ("destroy", destroy), ("saveAs", save)]

    init = ctypes.CDLL(str(library)).libreofficekit_hook_2
    init.argtypes, init.restype = [string, string], ptr
    kit = init(os.fsencode(library.parent), profile.as_uri().encode())
    if not kit:
        return False
    api = ctypes.cast(kit, ctypes.POINTER(ctypes.POINTER(KitClass))).contents.contents
    try:
        doc = api.documentLoad(kit, source.as_uri().encode())
        if not doc:
            return False
        document = ctypes.cast(doc, ctypes.POINTER(ctypes.POINTER(DocumentClass))).contents.contents
        try:
            return document.saveAs(doc, output.as_uri().encode(), output.suffix[1:].encode(), None)
        finally:
            document.destroy(doc)
    finally:
        api.destroy(kit)


if __name__ == "__main__":
    sys.exit(0 if kit_export(*map(Path, sys.argv[1:])) else 1)
