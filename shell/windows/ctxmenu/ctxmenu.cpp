// NimboCtxMenu.dll — Windows 11 modern context-menu handler for Nimbo.
//
// Implements IExplorerCommand: a top-level "Nimbo" command with sub-commands
// (Share / Version history / Always keep on this device / Free up space). Each
// leaf relaunches the packaged GUI exe with the matching verb and the selected
// path(s) — the same verbs the legacy registry submenu uses. The COM server is
// registered by the MSIX AppxManifest (windows.comServer + windows.fileExplorer
// ContextMenus), so this DLL only exposes DllGetClassObject / DllCanUnloadNow.
//
// Built with MinGW (w64devkit), statically linked so it has no runtime deps when
// loaded into explorer.exe.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shlobj.h>    // IShellItem
#include <shobjidl.h>  // IShellItemArray, SIGDN, IExplorerCommand
#include <shellapi.h>  // ShellExecuteW
#include <objbase.h>
#include <appmodel.h>  // GetCurrentApplicationUserModelId
#include <stdarg.h>
#include <new>
#include <string>

static HINSTANCE g_hInst = nullptr;
static LONG g_objs = 0;

// --- debug log (%TEMP%\nimbo-ctxmenu.log) ------------------------------------
static void dbg(const wchar_t* msg) {
    wchar_t path[MAX_PATH];
    DWORD n = GetTempPathW(MAX_PATH, path);
    lstrcpynW(path + n, L"nimbo-ctxmenu.log", (int)(MAX_PATH - n));
    HANDLE h = CreateFileW(path, FILE_APPEND_DATA, FILE_SHARE_READ | FILE_SHARE_WRITE,
                           nullptr, OPEN_ALWAYS, 0, nullptr);
    if (h == INVALID_HANDLE_VALUE) return;
    char out[2048];
    int c = WideCharToMultiByte(CP_UTF8, 0, msg, -1, out, sizeof(out) - 2, nullptr, nullptr);
    if (c > 0) {
        out[c - 1] = '\r'; out[c] = '\n';
        DWORD wr;
        WriteFile(h, out, c + 1, &wr, nullptr);
    }
    CloseHandle(h);
}
static void dbgf(const wchar_t* fmt, ...) {
    wchar_t buf[1024];
    va_list ap; va_start(ap, fmt);
    wvsprintfW(buf, fmt, ap);
    va_end(ap);
    dbg(buf);
}

// {7E9C1A20-3B4D-4F8A-A1C2-9D5E0F7B6A11}
static const CLSID CLSID_NimboMenu =
    {0x7e9c1a20, 0x3b4d, 0x4f8a, {0xa1, 0xc2, 0x9d, 0x5e, 0x0f, 0x7b, 0x6a, 0x11}};

// IExplorerCommand / IEnumExplorerCommand / EXPCMDSTATE / EXPCMDFLAGS and their
// IIDs come from <shobjidl.h>. We implement IExplorerCommand below; its vtable
// order is the SDK's, so the order of our overrides doesn't matter.

// CoTaskMem-allocated wide-string copy (Explorer frees the result).
static HRESULT TaskStrDup(const wchar_t* s, LPWSTR* out) {
    size_t n = (lstrlenW(s) + 1) * sizeof(wchar_t);
    *out = (LPWSTR)CoTaskMemAlloc(n);
    if (!*out) return E_OUTOFMEMORY;
    memcpy(*out, s, n);
    return S_OK;
}

struct Leaf { const wchar_t* title; const wchar_t* verb; };
static const Leaf kLeaves[] = {
    {L"Share",                      L"--share"},
    {L"Version history",            L"--versions"},
    {L"Always keep on this device", L"--keep"},
    {L"Free up space",              L"--free"},
};
static const int kLeafCount = 4;
static const int KIND_ROOT = -1;

// Full path to the packaged GUI exe (sits next to this DLL in the package).
static std::wstring ExePath() {
    wchar_t mod[MAX_PATH];
    GetModuleFileNameW(g_hInst, mod, MAX_PATH);
    wchar_t* slash = wcsrchr(mod, L'\\');
    if (slash) *slash = 0;
    return std::wstring(mod) + L"\\nimbo-gui.exe";
}

// AppUserModelId returns this (packaged) process's AUMID, or "" when unpackaged.
static std::wstring AppUserModelId() {
    UINT32 len = 0;
    GetCurrentApplicationUserModelId(&len, nullptr); // sets required length
    if (len == 0) return L"";
    std::wstring s(len, L'\0');
    if (GetCurrentApplicationUserModelId(&len, &s[0]) != 0) return L"";
    if (!s.empty() && s.back() == L'\0') s.pop_back();
    return s;
}

// Launch runs the Nimbo app once per selected item with "<verb> <path>". When
// packaged we MUST activate via the AppUserModelId (ApplicationActivationManager)
// so the launch carries package identity and reaches the running single instance
// — launching the WindowsApps exe by path starts a separate, identity-less copy
// that the verb never reaches. Unpackaged (loose dev exe) falls back to
// ShellExecute.
static void Launch(const wchar_t* verb, IShellItemArray* items) {
    if (!items) { dbg(L"Launch: null items"); return; }
    DWORD count = 0;
    items->GetCount(&count);
    std::wstring aumid = AppUserModelId();
    dbgf(L"Launch verb=%s aumid=%s count=%u", verb, aumid.c_str(), count);

    IApplicationActivationManager* aam = nullptr;
    if (!aumid.empty()) {
        HRESULT hr = CoCreateInstance(CLSID_ApplicationActivationManager, nullptr,
                                      CLSCTX_LOCAL_SERVER, IID_IApplicationActivationManager, (void**)&aam);
        if (FAILED(hr)) { dbgf(L"  CoCreate AAM 0x%08x", hr); aam = nullptr; }
    }
    std::wstring exe = ExePath();

    for (DWORD i = 0; i < count; ++i) {
        IShellItem* it = nullptr;
        if (FAILED(items->GetItemAt(i, &it)) || !it) continue;
        PWSTR path = nullptr;
        if (SUCCEEDED(it->GetDisplayName(SIGDN_FILESYSPATH, &path)) && path) {
            std::wstring args = std::wstring(verb) + L" \"" + path + L"\"";
            if (aam) {
                DWORD pid = 0;
                HRESULT ar = aam->ActivateApplication(aumid.c_str(), args.c_str(), AO_NONE, &pid);
                dbgf(L"  ActivateApplication %s -> 0x%08x pid=%lu", args.c_str(), (unsigned)ar, pid);
            } else {
                HINSTANCE r = ShellExecuteW(nullptr, L"open", exe.c_str(), args.c_str(), nullptr, SW_SHOWNORMAL);
                dbgf(L"  ShellExecute %s -> %p", args.c_str(), (void*)r);
            }
            CoTaskMemFree(path);
        } else {
            dbg(L"  GetDisplayName(FILESYSPATH) failed");
        }
        it->Release();
    }
    if (aam) aam->Release();
}

// --- The command (root or one leaf) ------------------------------------------

class Command : public IExplorerCommand {
public:
    explicit Command(int kind) : m_ref(1), m_kind(kind) { InterlockedIncrement(&g_objs); }
    virtual ~Command() { InterlockedDecrement(&g_objs); }

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_IExplorerCommand) {
            *ppv = static_cast<IExplorerCommand*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }

    // IExplorerCommand
    HRESULT STDMETHODCALLTYPE GetTitle(IShellItemArray*, LPWSTR* ppszName) override {
        const wchar_t* t = (m_kind == KIND_ROOT) ? L"Nimbo" : kLeaves[m_kind].title;
        return TaskStrDup(t, ppszName);
    }
    HRESULT STDMETHODCALLTYPE GetIcon(IShellItemArray*, LPWSTR* ppszIcon) override {
        if (m_kind != KIND_ROOT) { *ppszIcon = nullptr; return E_NOTIMPL; }
        std::wstring ico = ExePath() + L",0";
        return TaskStrDup(ico.c_str(), ppszIcon);
    }
    HRESULT STDMETHODCALLTYPE GetToolTip(IShellItemArray*, LPWSTR* ppszInfotip) override {
        *ppszInfotip = nullptr;
        return E_NOTIMPL;
    }
    HRESULT STDMETHODCALLTYPE GetCanonicalName(GUID* pguidCommandName) override {
        *pguidCommandName = GUID_NULL;
        return E_NOTIMPL;
    }
    HRESULT STDMETHODCALLTYPE GetState(IShellItemArray*, BOOL, EXPCMDSTATE* pCmdState) override {
        *pCmdState = ECS_ENABLED;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetFlags(EXPCMDFLAGS* pFlags) override {
        *pFlags = (m_kind == KIND_ROOT) ? ECF_HASSUBCOMMANDS : ECF_DEFAULT;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE EnumSubCommands(IEnumExplorerCommand** ppEnum) override;
    HRESULT STDMETHODCALLTYPE Invoke(IShellItemArray* psiItemArray, IBindCtx*) override {
        dbgf(L"Invoke kind=%d", m_kind);
        if (m_kind != KIND_ROOT) Launch(kLeaves[m_kind].verb, psiItemArray);
        return S_OK;
    }

private:
    LONG m_ref;
    int  m_kind;
};

// --- Sub-command enumerator ---------------------------------------------------

class Enum : public IEnumExplorerCommand {
public:
    Enum() : m_ref(1), m_idx(0) { InterlockedIncrement(&g_objs); }
    virtual ~Enum() { InterlockedDecrement(&g_objs); }

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_IEnumExplorerCommand) {
            *ppv = static_cast<IEnumExplorerCommand*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }
    HRESULT STDMETHODCALLTYPE Next(ULONG celt, IExplorerCommand** out, ULONG* fetched) override {
        dbgf(L"Enum::Next celt=%lu idx=%d", celt, m_idx);
        ULONG got = 0;
        for (; got < celt && m_idx < kLeafCount; ++got, ++m_idx) {
            Command* c = new (std::nothrow) Command(m_idx);
            if (!c) break;
            out[got] = c; // ref count 1, ownership transferred
        }
        if (fetched) *fetched = got;
        return (got == celt) ? S_OK : S_FALSE;
    }
    HRESULT STDMETHODCALLTYPE Skip(ULONG celt) override { m_idx += (int)celt; return S_OK; }
    HRESULT STDMETHODCALLTYPE Reset() override { m_idx = 0; return S_OK; }
    HRESULT STDMETHODCALLTYPE Clone(IEnumExplorerCommand** ppEnum) override {
        *ppEnum = nullptr;
        return E_NOTIMPL;
    }

private:
    LONG m_ref;
    int  m_idx;
};

HRESULT STDMETHODCALLTYPE Command::EnumSubCommands(IEnumExplorerCommand** ppEnum) {
    dbgf(L"EnumSubCommands kind=%d", m_kind);
    *ppEnum = nullptr;
    if (m_kind != KIND_ROOT) return E_NOTIMPL;
    Enum* e = new (std::nothrow) Enum();
    if (!e) return E_OUTOFMEMORY;
    *ppEnum = e;
    return S_OK;
}

// --- Class factory ------------------------------------------------------------

class Factory : public IClassFactory {
public:
    Factory() : m_ref(1) {}
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }
    HRESULT STDMETHODCALLTYPE CreateInstance(IUnknown* outer, REFIID riid, void** ppv) override {
        if (outer) return CLASS_E_NOAGGREGATION;
        Command* c = new (std::nothrow) Command(KIND_ROOT);
        if (!c) return E_OUTOFMEMORY;
        HRESULT hr = c->QueryInterface(riid, ppv);
        c->Release();
        return hr;
    }
    HRESULT STDMETHODCALLTYPE LockServer(BOOL) override { return S_OK; }

private:
    LONG m_ref;
};

// --- DLL exports --------------------------------------------------------------

extern "C" HRESULT STDMETHODCALLTYPE DllGetClassObject(REFCLSID rclsid, REFIID riid, void** ppv) {
    if (rclsid == CLSID_NimboMenu) {
        Factory* f = new (std::nothrow) Factory();
        if (!f) return E_OUTOFMEMORY;
        HRESULT hr = f->QueryInterface(riid, ppv);
        f->Release();
        return hr;
    }
    return CLASS_E_CLASSNOTAVAILABLE;
}

extern "C" HRESULT STDMETHODCALLTYPE DllCanUnloadNow() {
    return (g_objs == 0) ? S_OK : S_FALSE;
}

BOOL WINAPI DllMain(HINSTANCE hInst, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        g_hInst = hInst;
        DisableThreadLibraryCalls(hInst);
    }
    return TRUE;
}
