// NCOverlays.dll — Windows Explorer icon-overlay handlers for Nimbo.
//
// Three IShellIconOverlayIdentifier COM handlers (Synced / Syncing / Warning).
// Each IsMemberOf() asks the running Nimbo app — over the named pipe
// \\.\pipe\Nimbo-overlay — for the file's status and claims the file if it
// matches that handler's state. Built with MinGW (w64devkit), statically linked
// so it has no runtime dependencies when loaded into explorer.exe.
//
// Registration (run elevated):  regsvr32 NCOverlays.dll
// Removal:                      regsvr32 /u NCOverlays.dll
// Explorer must be restarted to pick up overlay-handler changes.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shlobj.h>
#include <olectl.h>
#include <string>
#include <map>
#include <new>

static HINSTANCE g_hInst = nullptr;
static LONG g_objs = 0;          // live COM object count
static CRITICAL_SECTION g_cs;    // guards g_cache

// --- Three handler identities -------------------------------------------------

// {4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0001} Synced
static const CLSID CLSID_OK   = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x00,0x01}};
// {4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0002} Syncing
static const CLSID CLSID_SYNC = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x00,0x02}};
// {4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0003} Warning
static const CLSID CLSID_WARN = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x00,0x03}};
// {4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0004} Shared (with/by another user)
static const CLSID CLSID_SHARE = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x00,0x04}};

struct HandlerDef {
    const CLSID*  clsid;
    const wchar_t* clsidStr;   // registry string form
    const wchar_t* regName;    // ShellIconOverlayIdentifiers key (leading spaces win priority)
    const wchar_t* friendly;
    const wchar_t* state;      // pipe state this handler matches
    const wchar_t* iconFile;   // .ico filename next to the DLL
};

static const HandlerDef kHandlers[] = {
    {&CLSID_OK,   L"{4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0001}", L"Nimbo1Synced",  L"Nimbo Synced",  L"OK",   L"ok.ico"},
    {&CLSID_SYNC, L"{4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0002}", L"Nimbo2Syncing", L"Nimbo Syncing", L"SYNC", L"sync.ico"},
    {&CLSID_WARN, L"{4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0003}", L"Nimbo3Warning", L"Nimbo Warning", L"WARN", L"warn.ico"},
    {&CLSID_SHARE, L"{4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0004}", L"Nimbo4Shared", L"Nimbo Shared", L"SHARED", L"shared.ico"},
};
static const int kHandlerCount = 4;

// Windows honors only ~15 overlay handlers, claimed in name sort order — and
// fewer than the registry list suggests once built-ins take their share.
// OneDrive escalated to 19 leading spaces (it used 17 when this was 18), and
// at 18 the budget cut off EXACTLY after our third handler: the shared badge
// registered, activated under a direct COM probe, and never painted
// (2026-08-21, the full night of it). 20 sorts all four of ours first.
static const int kPrioritySpaces = 20;

// --- Named-pipe status query (with a short cache) -----------------------------

static const wchar_t* kPipe = L"\\\\.\\pipe\\Nimbo-overlay";

struct CacheEntry { std::wstring state; ULONGLONG tick; };
static std::map<std::wstring, CacheEntry> g_cache;

static std::string Narrow(const wchar_t* w) {
    int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, nullptr, 0, nullptr, nullptr);
    if (n <= 0) return std::string();
    std::string s(n - 1, '\0');
    WideCharToMultiByte(CP_UTF8, 0, w, -1, &s[0], n, nullptr, nullptr);
    return s;
}

// Query the app for a path's state. Returns L"OK"/L"SYNC"/L"WARN"/L"NONE" or
// empty on failure (app not running).
static std::wstring QueryPipe(const wchar_t* path) {
    HANDLE h = INVALID_HANDLE_VALUE;
    for (int attempt = 0; attempt < 2; ++attempt) {
        h = CreateFileW(kPipe, GENERIC_READ | GENERIC_WRITE, 0, nullptr, OPEN_EXISTING, 0, nullptr);
        if (h != INVALID_HANDLE_VALUE) break;
        if (GetLastError() != ERROR_PIPE_BUSY) return L"";
        if (!WaitNamedPipeW(kPipe, 150)) return L"";
    }
    if (h == INVALID_HANDLE_VALUE) return L"";

    std::string req = "RETRIEVE_FILE_STATUS:" + Narrow(path) + "\n";
    DWORD wrote = 0;
    WriteFile(h, req.data(), (DWORD)req.size(), &wrote, nullptr);

    std::string resp;
    char buf[512];
    DWORD rd = 0;
    while (ReadFile(h, buf, sizeof(buf), &rd, nullptr) && rd > 0) {
        resp.append(buf, rd);
        if (resp.find('\n') != std::string::npos) break;
        if (resp.size() > 4096) break;
    }
    CloseHandle(h);

    // Expected: "STATUS:<STATE>:<path>\n"
    if (resp.compare(0, 7, "STATUS:") != 0) return L"";
    size_t a = 7, b = resp.find(':', a);
    if (b == std::string::npos) return L"";
    std::string st = resp.substr(a, b - a);
    return std::wstring(st.begin(), st.end());
}

static std::wstring QueryCached(const wchar_t* path) {
    std::wstring key(path);
    ULONGLONG now = GetTickCount64();
    EnterCriticalSection(&g_cs);
    auto it = g_cache.find(key);
    if (it != g_cache.end() && now - it->second.tick < 1500) {
        std::wstring v = it->second.state;
        LeaveCriticalSection(&g_cs);
        return v;
    }
    LeaveCriticalSection(&g_cs);

    std::wstring v = QueryPipe(path);

    EnterCriticalSection(&g_cs);
    g_cache[key] = CacheEntry{v, now};
    if (g_cache.size() > 4096) g_cache.clear(); // crude bound
    LeaveCriticalSection(&g_cs);
    return v;
}

// --- The overlay handler ------------------------------------------------------

class Overlay : public IShellIconOverlayIdentifier {
public:
    explicit Overlay(const HandlerDef* def) : m_ref(1), m_def(def) { InterlockedIncrement(&g_objs); }
    virtual ~Overlay() { InterlockedDecrement(&g_objs); }

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_IShellIconOverlayIdentifier) {
            *ppv = static_cast<IShellIconOverlayIdentifier*>(this);
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

    // IShellIconOverlayIdentifier
    HRESULT STDMETHODCALLTYPE IsMemberOf(LPCWSTR pwszPath, DWORD /*dwAttrib*/) override {
        std::wstring st = QueryCached(pwszPath);
        return (st == m_def->state) ? S_OK : S_FALSE;
    }
    HRESULT STDMETHODCALLTYPE GetOverlayInfo(LPWSTR pwszIconFile, int cchMax, int* pIndex, DWORD* pdwFlags) override {
        wchar_t mod[MAX_PATH];
        GetModuleFileNameW(g_hInst, mod, MAX_PATH);
        // Replace the DLL filename with icons\<file>.
        wchar_t* slash = wcsrchr(mod, L'\\');
        if (slash) *slash = 0;
        wchar_t full[MAX_PATH];
        wsprintfW(full, L"%s\\icons\\%s", mod, m_def->iconFile);
        lstrcpynW(pwszIconFile, full, cchMax);
        *pIndex = 0;
        *pdwFlags = ISIOI_ICONFILE | ISIOI_ICONINDEX;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetPriority(int* pPriority) override { *pPriority = 0; return S_OK; }

private:
    LONG m_ref;
    const HandlerDef* m_def;
};

#include "propsource.inc.cpp"

// --- Null cloud-files handlers ------------------------------------------------
//
// The package manifest's windows.cloudFiles extension must declare a
// CustomStateHandler / ThumbnailProviderHandler / ExtendedPropertyHandler /
// BannersHandler (the schema requires all four elements), and the Clsid must
// resolve to a REAL activatable class: with made-up CLSIDs Explorer's per-item
// state pipeline in the sync root failed mid-query and drew wrong or stuck
// Status glyphs, and with the Clsid attributes omitted Windows invalidated the
// whole sync-root registration (GetCurrentSyncRoots stopped listing it - no
// Status column, no cloud glyphs at all; measured on the test VM, v0.1.0.230/1).
//
// Nimbo has no custom states, thumbnails, properties, or banners - the native
// cloud glyphs are the UI - so these classes activate successfully and then
// decline every interface beyond IUnknown, which Explorer handles as "handler
// present, nothing to add" and falls back to native rendering cleanly.

static const CLSID CLSID_NullCustomState = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x01,0x01}};
static const CLSID CLSID_NullThumbnail   = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x01,0x02}};
static const CLSID CLSID_NullExtProps    = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x01,0x03}};
static const CLSID CLSID_NullBanners     = {0x4f8b2c10,0x1a3d,0x4e55,{0x9b,0x21,0x0c,0x7e,0x5a,0x9d,0x01,0x04}};

class NullCloudHandler : public IUnknown {
public:
    NullCloudHandler() : m_ref(1) { InterlockedIncrement(&g_objs); }
    virtual ~NullCloudHandler() { InterlockedDecrement(&g_objs); }
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown) {
            *ppv = static_cast<IUnknown*>(this);
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
private:
    LONG m_ref;
};

class NullFactory : public IClassFactory {
public:
    NullFactory() : m_ref(1) {}
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
        NullCloudHandler* o = new (std::nothrow) NullCloudHandler();
        if (!o) return E_OUTOFMEMORY;
        HRESULT hr = o->QueryInterface(riid, ppv);
        o->Release();
        return hr;
    }
    HRESULT STDMETHODCALLTYPE LockServer(BOOL) override { return S_OK; }
private:
    LONG m_ref;
};

// --- Class factory ------------------------------------------------------------

class Factory : public IClassFactory {
public:
    explicit Factory(const HandlerDef* def) : m_ref(1), m_def(def) {}
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
        Overlay* o = new (std::nothrow) Overlay(m_def);
        if (!o) return E_OUTOFMEMORY;
        HRESULT hr = o->QueryInterface(riid, ppv);
        o->Release();
        return hr;
    }
    HRESULT STDMETHODCALLTYPE LockServer(BOOL) override { return S_OK; }
private:
    LONG m_ref;
    const HandlerDef* m_def;
};

// --- Registry helpers ---------------------------------------------------------

static LONG SetReg(HKEY root, const wchar_t* subkey, const wchar_t* name, const wchar_t* val) {
    HKEY k;
    LONG r = RegCreateKeyExW(root, subkey, 0, nullptr, 0, KEY_WRITE, nullptr, &k, nullptr);
    if (r != ERROR_SUCCESS) return r;
    r = RegSetValueExW(k, name, 0, REG_SZ, (const BYTE*)val, (DWORD)((lstrlenW(val) + 1) * sizeof(wchar_t)));
    RegCloseKey(k);
    return r;
}

static const wchar_t* kOverlayRoot =
    L"SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Explorer\\ShellIconOverlayIdentifiers\\";

// --- COM DLL exports ----------------------------------------------------------

extern "C" HRESULT STDMETHODCALLTYPE DllGetClassObject(REFCLSID rclsid, REFIID riid, void** ppv) {
    for (int i = 0; i < kHandlerCount; ++i) {
        if (rclsid == *kHandlers[i].clsid) {
            Factory* f = new (std::nothrow) Factory(&kHandlers[i]);
            if (!f) return E_OUTOFMEMORY;
            HRESULT hr = f->QueryInterface(riid, ppv);
            f->Release();
            return hr;
        }
    }
    if (rclsid == CLSID_NullCustomState) {
        // The real CustomStateHandler: per-item Status-column state on every
        // install channel (see propsource.inc.cpp).
        PropertySourceFactory* f = new (std::nothrow) PropertySourceFactory();
        if (!f) return E_OUTOFMEMORY;
        HRESULT hr = f->QueryInterface(riid, ppv);
        f->Release();
        return hr;
    }
    if (rclsid == CLSID_NullThumbnail ||
        rclsid == CLSID_NullExtProps || rclsid == CLSID_NullBanners) {
        NullFactory* f = new (std::nothrow) NullFactory();
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

extern "C" HRESULT STDMETHODCALLTYPE DllRegisterServer() {
    wchar_t mod[MAX_PATH];
    GetModuleFileNameW(g_hInst, mod, MAX_PATH);
    for (int i = 0; i < kHandlerCount; ++i) {
        const HandlerDef& h = kHandlers[i];
        wchar_t key[256];
        wsprintfW(key, L"CLSID\\%s", h.clsidStr);
        if (SetReg(HKEY_CLASSES_ROOT, key, nullptr, h.friendly) != ERROR_SUCCESS) return SELFREG_E_CLASS;
        wsprintfW(key, L"CLSID\\%s\\InprocServer32", h.clsidStr);
        SetReg(HKEY_CLASSES_ROOT, key, nullptr, mod);
        SetReg(HKEY_CLASSES_ROOT, key, L"ThreadingModel", L"Apartment");

        std::wstring ov = std::wstring(kOverlayRoot) + std::wstring(kPrioritySpaces, L' ') + h.regName;
        if (SetReg(HKEY_LOCAL_MACHINE, ov.c_str(), nullptr, h.clsidStr) != ERROR_SUCCESS) return SELFREG_E_CLASS;
    }
    SHChangeNotify(SHCNE_ASSOCCHANGED, SHCNF_IDLIST, nullptr, nullptr);
    return S_OK;
}

extern "C" HRESULT STDMETHODCALLTYPE DllUnregisterServer() {
    for (int i = 0; i < kHandlerCount; ++i) {
        const HandlerDef& h = kHandlers[i];
        wchar_t key[256];
        wsprintfW(key, L"CLSID\\%s", h.clsidStr);
        RegDeleteTreeW(HKEY_CLASSES_ROOT, key);
        std::wstring ov = std::wstring(kOverlayRoot) + std::wstring(kPrioritySpaces, L' ') + h.regName;
        RegDeleteTreeW(HKEY_LOCAL_MACHINE, ov.c_str());
    }
    SHChangeNotify(SHCNE_ASSOCCHANGED, SHCNF_IDLIST, nullptr, nullptr);
    return S_OK;
}

BOOL WINAPI DllMain(HINSTANCE hInst, DWORD reason, LPVOID) {
    switch (reason) {
    case DLL_PROCESS_ATTACH:
        g_hInst = hInst;
        DisableThreadLibraryCalls(hInst);
        InitializeCriticalSection(&g_cs);
        break;
    case DLL_PROCESS_DETACH:
        DeleteCriticalSection(&g_cs);
        break;
    }
    return TRUE;
}
