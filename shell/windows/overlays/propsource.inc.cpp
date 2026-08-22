// propsource.inc.cpp — included by overlays.cpp. NOT a standalone TU.
//
// Cloud-files CustomStateHandler: IStorageProviderItemPropertySource behind
// CLSID ...0101 (the manifest's CustomStateHandler). Explorer calls
// GetItemProperties(path) for items inside our cloud sync root and renders the
// returned property's IconResource in the Status column. This is the only
// per-item state mechanism that works identically on every install channel
// (Store included): no elevation, no HKLM, declared by the MSIX manifest.
//
// Measured facts that led here (2026-08-20): classic HKLM overlay badges need
// Setup's elevated step and are suppressed inside cloud roots; the manifest
// DesktopIconOverlayHandlers draw in neither plain folders nor valid roots;
// the native Status glyphs stay blank on an AlwaysFull status root.
//
// State comes from the same Nimbo-overlay pipe as the badges (QueryCached):
// OK -> Synced, SYNC -> Syncing, WARN -> Attention, NONE/empty -> nothing.

#include <inspectable.h>
#include <hstring.h>

#ifndef E_BOUNDS
#define E_BOUNDS ((HRESULT)0x8000000BL)
#endif

// {8F6F9C3E-F632-4A9B-8D99-D2D7A11DF56A} IStorageProviderItemPropertySource
static const IID IID_ItemPropertySource = {0x8f6f9c3e,0xf632,0x4a9b,{0x8d,0x99,0xd2,0xd7,0xa1,0x1d,0xf5,0x6a}};
// {4584CB69-EE26-59E0-B05D-C9A7851A7317} IIterable<StorageProviderItemProperty>
static const IID IID_PropIterable = {0x4584cb69,0xee26,0x59e0,{0xb0,0x5d,0xc9,0xa7,0x85,0x1a,0x73,0x17}};
// {0C6DDDDE-1AA3-54F5-B139-E4A237DC1C5F} IIterator<StorageProviderItemProperty>
static const IID IID_PropIterator = {0x0c6dddde,0x1aa3,0x54f5,{0xb1,0x39,0xe4,0xa2,0x37,0xdc,0x1c,0x5f}};
// {476CB558-730B-4188-B7B5-63B716ED476D} IStorageProviderItemProperty
static const IID IID_ItemProperty = {0x476cb558,0x730b,0x4188,{0xb7,0xb5,0x63,0xb7,0x16,0xed,0x47,0x6d}};
// IInspectable
static const IID IID_Inspectable = {0xAF86E2E0,0xB12D,0x4c6a,{0x9C,0x5A,0xD7,0xAA,0x65,0x10,0x1E,0x90}};
// {94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90} IAgileObject (no methods beyond IUnknown)
static const IID IID_AgileObject = {0x94ea2b94,0xe9cc,0x49e0,{0xc0,0xff,0xee,0x64,0xca,0x8f,0x5b,0x90}};
// {00000003-0000-0000-C000-000000000046} IMarshal
static const IID IID_Marshal = {0x00000003,0x0000,0x0000,{0xc0,0x00,0x00,0x00,0x00,0x00,0x00,0x46}};

// combase bound dynamically: the DLL keeps loading even where WinRT is absent
// (the handler then fails activation gracefully).
typedef HRESULT (WINAPI *PFN_WindowsCreateString)(PCWSTR, UINT32, HSTRING*);
typedef HRESULT (WINAPI *PFN_WindowsDeleteString)(HSTRING);
typedef PCWSTR  (WINAPI *PFN_WindowsGetStringRawBuffer)(HSTRING, UINT32*);
typedef HRESULT (WINAPI *PFN_RoActivateInstance)(HSTRING, IInspectable**);
static PFN_WindowsCreateString       pWindowsCreateString = nullptr;
static PFN_WindowsDeleteString       pWindowsDeleteString = nullptr;
static PFN_WindowsGetStringRawBuffer pWindowsGetStringRawBuffer = nullptr;
static PFN_RoActivateInstance        pRoActivateInstance = nullptr;

static bool EnsureWinRT() {
    if (pRoActivateInstance) return true;
    HMODULE m = LoadLibraryW(L"combase.dll");
    if (!m) return false;
    pWindowsCreateString = (PFN_WindowsCreateString)GetProcAddress(m, "WindowsCreateString");
    pWindowsDeleteString = (PFN_WindowsDeleteString)GetProcAddress(m, "WindowsDeleteString");
    pWindowsGetStringRawBuffer = (PFN_WindowsGetStringRawBuffer)GetProcAddress(m, "WindowsGetStringRawBuffer");
    pRoActivateInstance = (PFN_RoActivateInstance)GetProcAddress(m, "RoActivateInstance");
    return pWindowsCreateString && pWindowsDeleteString && pWindowsGetStringRawBuffer && pRoActivateInstance;
}

// Raw vtable dispatch for WinRT objects we activate but do not implement.
template <typename... A>
static HRESULT VtCall(void* obj, int idx, A... args) {
    typedef HRESULT (STDMETHODCALLTYPE *Fn)(void*, A...);
    Fn f = (Fn)((*(void***)obj)[idx]);
    return f(obj, args...);
}

// MakeItemProperty activates Windows.Storage.Provider.StorageProviderItemProperty
// and fills Id/Value/IconResource. Caller releases; null on failure.
// IStorageProviderItemProperty vtable (IDL declaration order, put before get):
// IInspectable 0-5, put_Id=6, get_Id=7, put_Value=8, get_Value=9,
// put_IconResource=10, get_IconResource=11.
static IUnknown* MakeItemProperty(INT32 id, const wchar_t* value, const wchar_t* iconRes) {
    if (!EnsureWinRT()) return nullptr;
    static const wchar_t* kCls = L"Windows.Storage.Provider.StorageProviderItemProperty";
    HSTRING cls = nullptr;
    if (FAILED(pWindowsCreateString(kCls, (UINT32)wcslen(kCls), &cls))) return nullptr;
    IInspectable* insp = nullptr;
    HRESULT hr = pRoActivateInstance(cls, &insp);
    pWindowsDeleteString(cls);
    if (FAILED(hr) || !insp) return nullptr;
    void* prop = nullptr;
    hr = insp->QueryInterface(IID_ItemProperty, &prop);
    insp->Release();
    if (FAILED(hr) || !prop) return nullptr;
    VtCall(prop, 6, id);
    HSTRING hv = nullptr;
    pWindowsCreateString(value, (UINT32)wcslen(value), &hv);
    VtCall(prop, 8, hv);
    pWindowsDeleteString(hv);
    HSTRING hi = nullptr;
    pWindowsCreateString(iconRes, (UINT32)wcslen(iconRes), &hi);
    VtCall(prop, 10, hi);
    pWindowsDeleteString(hi);
    return (IUnknown*)prop;
}

// ABI interface shapes, declared EXACTLY as the vtables Explorer expects:
// IUnknown 0-2, IInspectable 3-5, interface methods from 6. Implementation
// classes derive from these and override — they must NOT declare a virtual
// destructor or any other new virtual BEFORE the interface methods. The .245
// version derived from bare IUnknown, added the IInspectable methods as new
// virtuals, and put a virtual destructor first: on the Itanium ABI (MinGW)
// that destructor occupies TWO vtable slots at its declaration position, so
// every method from slot 3 up was off by two — Explorer's
// GetRuntimeClassName call (slot 4) invoked the deleting destructor, its
// custom-state cache then "succeeded" with a null iterable, and
// CSyncRootManager::GetStorageProviderCustomStatesFromPath dereferenced it
// unchecked: an Explorer crash loop on every folder open (crash dump,
// 2026-08-20). Deletion happens only via Release() on the concrete class, so
// the destructors are deliberately non-virtual.
struct ABIInspectable : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetIids(ULONG*, IID**) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetRuntimeClassName(HSTRING*) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetTrustLevel(int*) = 0;
};
struct ABIPropIterator : public ABIInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_Current(void**) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_HasCurrent(unsigned char*) = 0;
    virtual HRESULT STDMETHODCALLTYPE MoveNext(unsigned char*) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetMany(ULONG, void**, ULONG*) = 0;
};
struct ABIPropIterable : public ABIInspectable {
    virtual HRESULT STDMETHODCALLTYPE First(void**) = 0;
};
struct ABIPropertySource : public ABIInspectable {
    virtual HRESULT STDMETHODCALLTYPE GetItemProperties(HSTRING, void**) = 0;
};

// Explorer's CSyncRootManagerCache hands these objects between its shell task
// threads via agile references, so each object aggregates the free-threaded
// marshaler, answers IAgileObject, and reports a real runtime class name —
// a non-agile object degrades that wrapping instead of failing it cleanly.
static HRESULT AgileQI(IUnknown* self, IUnknown* ftm, REFIID riid, void** ppv) {
    if (riid == IID_AgileObject) {
        *ppv = self;
        self->AddRef();
        return S_OK;
    }
    if (riid == IID_Marshal && ftm) return ftm->QueryInterface(riid, ppv);
    *ppv = nullptr;
    return E_NOINTERFACE;
}

static HRESULT MakeClassName(const wchar_t* name, HSTRING* out) {
    if (!EnsureWinRT()) { *out = nullptr; return E_NOTIMPL; }
    return pWindowsCreateString(name, (UINT32)wcslen(name), out);
}

static const wchar_t kIterableName[] = L"Windows.Foundation.Collections.IIterable`1<Windows.Storage.Provider.StorageProviderItemProperty>";
static const wchar_t kIteratorName[] = L"Windows.Foundation.Collections.IIterator`1<Windows.Storage.Provider.StorageProviderItemProperty>";
static const wchar_t kSourceName[]   = L"NimboOverlays.PropertySource";

// One-element (or empty) iterator/iterable over the activated property.
class PropIterator : public ABIPropIterator {
public:
    explicit PropIterator(IUnknown* item) : m_ref(1), m_ftm(nullptr), m_item(item), m_first(true) {
        if (m_item) m_item->AddRef();
        CoCreateFreeThreadedMarshaler(static_cast<IUnknown*>(this), &m_ftm);
        InterlockedIncrement(&g_objs);
    }
    ~PropIterator() {
        if (m_ftm) m_ftm->Release();
        if (m_item) m_item->Release();
        InterlockedDecrement(&g_objs);
    }
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_Inspectable || riid == IID_PropIterator) {
            *ppv = static_cast<ABIPropIterator*>(this);
            AddRef();
            return S_OK;
        }
        return AgileQI(this, m_ftm, riid, ppv);
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }
    HRESULT STDMETHODCALLTYPE GetIids(ULONG* c, IID** iids) override {
        *c = 1;
        *iids = (IID*)CoTaskMemAlloc(sizeof(IID));
        if (!*iids) return E_OUTOFMEMORY;
        (*iids)[0] = IID_PropIterator;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetRuntimeClassName(HSTRING* n) override { return MakeClassName(kIteratorName, n); }
    HRESULT STDMETHODCALLTYPE GetTrustLevel(int* t) override { *t = 0; return S_OK; }
    HRESULT STDMETHODCALLTYPE get_Current(void** cur) override {
        if (!m_item || !m_first) { *cur = nullptr; return E_BOUNDS; }
        m_item->AddRef();
        *cur = m_item;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE get_HasCurrent(unsigned char* has) override {
        *has = (m_item && m_first) ? 1 : 0;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE MoveNext(unsigned char* has) override {
        m_first = false;
        *has = 0;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetMany(ULONG cap, void** items, ULONG* got) override {
        *got = 0;
        if (m_item && m_first && cap > 0) {
            m_item->AddRef();
            items[0] = m_item;
            *got = 1;
            m_first = false;
        }
        return S_OK;
    }
private:
    LONG m_ref;
    IUnknown* m_ftm;
    IUnknown* m_item;
    bool m_first;
};

class PropIterable : public ABIPropIterable {
public:
    explicit PropIterable(IUnknown* item) : m_ref(1), m_ftm(nullptr), m_item(item) {
        if (m_item) m_item->AddRef();
        CoCreateFreeThreadedMarshaler(static_cast<IUnknown*>(this), &m_ftm);
        InterlockedIncrement(&g_objs);
    }
    ~PropIterable() {
        if (m_ftm) m_ftm->Release();
        if (m_item) m_item->Release();
        InterlockedDecrement(&g_objs);
    }
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_Inspectable || riid == IID_PropIterable) {
            *ppv = static_cast<ABIPropIterable*>(this);
            AddRef();
            return S_OK;
        }
        return AgileQI(this, m_ftm, riid, ppv);
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }
    HRESULT STDMETHODCALLTYPE GetIids(ULONG* c, IID** iids) override {
        *c = 1;
        *iids = (IID*)CoTaskMemAlloc(sizeof(IID));
        if (!*iids) return E_OUTOFMEMORY;
        (*iids)[0] = IID_PropIterable;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetRuntimeClassName(HSTRING* n) override { return MakeClassName(kIterableName, n); }
    HRESULT STDMETHODCALLTYPE GetTrustLevel(int* t) override { *t = 0; return S_OK; }
    HRESULT STDMETHODCALLTYPE First(void** it) override {
        PropIterator* i = new (std::nothrow) PropIterator(m_item);
        if (!i) { *it = nullptr; return E_OUTOFMEMORY; }
        *it = static_cast<ABIPropIterator*>(i);
        return S_OK;
    }
private:
    LONG m_ref;
    IUnknown* m_ftm;
    IUnknown* m_item;
};

class PropertySource : public ABIPropertySource {
public:
    PropertySource() : m_ref(1), m_ftm(nullptr) {
        CoCreateFreeThreadedMarshaler(static_cast<IUnknown*>(this), &m_ftm);
        InterlockedIncrement(&g_objs);
    }
    ~PropertySource() {
        if (m_ftm) m_ftm->Release();
        InterlockedDecrement(&g_objs);
    }
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_Inspectable || riid == IID_ItemPropertySource) {
            *ppv = static_cast<ABIPropertySource*>(this);
            AddRef();
            return S_OK;
        }
        return AgileQI(this, m_ftm, riid, ppv);
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&m_ref); }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG r = InterlockedDecrement(&m_ref);
        if (r == 0) delete this;
        return r;
    }
    HRESULT STDMETHODCALLTYPE GetIids(ULONG* c, IID** iids) override {
        *c = 1;
        *iids = (IID*)CoTaskMemAlloc(sizeof(IID));
        if (!*iids) return E_OUTOFMEMORY;
        (*iids)[0] = IID_ItemPropertySource;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetRuntimeClassName(HSTRING* n) override { return MakeClassName(kSourceName, n); }
    HRESULT STDMETHODCALLTYPE GetTrustLevel(int* t) override { *t = 0; return S_OK; }
    HRESULT STDMETHODCALLTYPE GetItemProperties(HSTRING itemPath, void** result) override {
        *result = nullptr;
        if (!EnsureWinRT()) return E_NOTIMPL;
        UINT32 len = 0;
        PCWSTR path = pWindowsGetStringRawBuffer(itemPath, &len);
        std::wstring st = path ? QueryCached(path) : L"";
        IUnknown* item = nullptr;
        wchar_t mod[MAX_PATH];
        GetModuleFileNameW(g_hInst, mod, MAX_PATH);
        wchar_t icon[MAX_PATH + 16];
        if (st == L"OK") {
            wsprintfW(icon, L"%s,-101", mod);
            item = MakeItemProperty(1, L"Synced", icon);
        } else if (st == L"SYNC") {
            wsprintfW(icon, L"%s,-102", mod);
            item = MakeItemProperty(2, L"Syncing", icon);
        } else if (st == L"WARN") {
            wsprintfW(icon, L"%s,-103", mod);
            item = MakeItemProperty(3, L"Attention", icon);
        } else if (st == L"SHARED") {
            wsprintfW(icon, L"%s,-104", mod);
            item = MakeItemProperty(4, L"Shared", icon);
        }
        PropIterable* it = new (std::nothrow) PropIterable(item);
        if (item) item->Release();
        if (!it) return E_OUTOFMEMORY;
        *result = static_cast<ABIPropIterable*>(it);
        return S_OK;
    }
private:
    LONG m_ref;
    IUnknown* m_ftm;
};

class PropertySourceFactory : public IClassFactory {
public:
    PropertySourceFactory() : m_ref(1) {}
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (riid == IID_IUnknown || riid == IID_IClassFactory) { *ppv = this; AddRef(); return S_OK; }
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
        PropertySource* o = new (std::nothrow) PropertySource();
        if (!o) return E_OUTOFMEMORY;
        HRESULT hr = o->QueryInterface(riid, ppv);
        o->Release();
        return hr;
    }
    HRESULT STDMETHODCALLTYPE LockServer(BOOL) override { return S_OK; }
private:
    LONG m_ref;
};
