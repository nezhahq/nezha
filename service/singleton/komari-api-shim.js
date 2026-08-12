/* Nezha -> Komari public API compatibility layer. Read-only by design. */
;(() => {
  'use strict';
  if (window.__NEZHA_KOMARI_API_SHIM__) return;
  window.__NEZHA_KOMARI_API_SHIM__ = '1.0.0';

  const NativeFetch = window.fetch.bind(window);
  const NativeXMLHttpRequest = window.XMLHttpRequest;
  const NativeWebSocket = window.WebSocket;
  const cfg = window.__NEZHA_KOMARI_COMPAT__ || {};
  const state = { snapshot: null, snapshotPromise: null, settings: null, groups: null, listeners: new Set() };
  const maxMetricRequests = 24;
  const jsonHeaders = { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' };
  const metricMap = {
    'cpu.usage': 'cpu', 'load.average': 'load1', 'memory.used': 'memory',
    'swap.used': 'swap', 'disk.used': 'disk', 'net.in.rate': 'net_in_speed',
    'net.out.rate': 'net_out_speed', 'net.total.down': 'net_in_transfer',
    'net.total.up': 'net_out_transfer', 'process.count': 'process_count',
    'connections.tcp': 'tcp_conn', 'connections.udp': 'udp_conn',
    'uptime': 'uptime', 'temperature': 'temperature', 'gpu.usage': 'gpu'
  };

  const asURL = input => new URL(typeof input === 'string' ? input : input.url, location.href);
  const response = (data, status = 200) => new Response(JSON.stringify(data), { status, headers: jsonHeaders });
  const rpcOK = (id, result) => ({ jsonrpc: '2.0', id: id == null ? null : id, result });
  const rpcError = (id, code, message) => ({ jsonrpc: '2.0', id: id == null ? null : id, error: { code, message } });
  const readonlyError = id => rpcError(id, -32601, 'Komari compatibility layer is read-only');
  const unwrap = async r => {
    const body = await r.json();
    if (!r.ok || body?.success === false || body?.error) throw new Error(body?.error || `HTTP ${r.status}`);
    return body?.data === undefined ? body : body.data;
  };
  const nativeJSON = async path => unwrap(await NativeFetch(path, { credentials: 'same-origin', cache: 'no-store' }));
  const iso = value => {
    const d = new Date(value || Date.now());
    return Number.isNaN(d.getTime()) ? new Date().toISOString() : d.toISOString();
  };
  const num = value => Number.isFinite(Number(value)) ? Number(value) : 0;

  if (cfg.wallpaper) {
    Object.defineProperty(window, 'CustomBackgroundImage', { configurable: false, enumerable: true, writable: false, value: cfg.wallpaper });
    Object.defineProperty(window, 'CustomMobileBackgroundImage', { configurable: false, enumerable: true, writable: false, value: cfg.wallpaper });
  }

  const isKomariAdminPath = value => {
    const path = new URL(String(value || location.href), location.href).pathname.replace(/\/+$/, '') || '/';
    return path === '/admin' || path === '/admin/login' || path === '/login';
  };
  const redirectKomariAdmin = value => {
    if (!isKomariAdminPath(value)) return false;
    location.assign('/dashboard');
    return true;
  };
  const loginLabels = new Set(['login', 'sign in', 'log in', '登录', '登入', 'ログイン']);
  const isKomariLoginControl = element => {
    const control = element?.closest?.('a[href],button,[role="button"]');
    if (!control) return false;
    if (control.matches('a[href]') && isKomariAdminPath(control.href)) return true;
    const aria = String(control.getAttribute('aria-label') || '').trim().toLowerCase();
    const title = String(control.getAttribute('title') || '').trim().toLowerCase();
    const text = String(control.innerText || control.textContent || '').trim().toLowerCase();
    return loginLabels.has(aria) || loginLabels.has(title) || loginLabels.has(text);
  };
  document.addEventListener('click', event => {
    const control = event.target?.closest?.('a[href],button,[role="button"]');
    if (!control || !isKomariLoginControl(control)) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    location.assign('/dashboard');
  }, true);
  const nativePushState = history.pushState.bind(history);
  const nativeReplaceState = history.replaceState.bind(history);
  history.pushState = function(stateValue, unused, url) {
    if (url != null && redirectKomariAdmin(url)) return;
    return nativePushState(stateValue, unused, url);
  };
  history.replaceState = function(stateValue, unused, url) {
    if (url != null && redirectKomariAdmin(url)) return;
    return nativeReplaceState(stateValue, unused, url);
  };
  window.addEventListener('popstate', () => redirectKomariAdmin(location.href));
  if (isKomariAdminPath(location.href)) queueMicrotask(() => location.assign('/dashboard'));

  async function groups() {
    if (state.groups) return state.groups;
    try {
      const rows = await nativeJSON('/api/v1/server-group');
      state.groups = {};
      (rows || []).forEach(row => (row.servers || []).forEach(id => { state.groups[String(id)] = row.group?.name || 'Nezha'; }));
    } catch (_) { state.groups = {}; }
    return state.groups;
  }

  function nodeFromServer(s) {
    const h = s?.host || {}, st = s?.state || {};
    return {
      uuid: String(s?.id ?? ''), name: String(s?.name || 'Unnamed'), cpu_name: (h.cpu || []).join(' / '),
      virtualization: h.virtualization || '', arch: h.arch || '', cpu_cores: (h.cpu || []).length,
      cpu_physical_cores: (h.cpu || []).length, os: h.platform || '', kernel_version: h.platform_version || '',
      gpu_name: (h.gpu || []).join(' / '), region: s?.country_code || '', mem_total: num(h.mem_total),
      swap_total: num(h.swap_total), disk_total: num(h.disk_total), version: h.version || '',
      weight: num(s?.display_index), price: 0, billing_cycle: 0, auto_renewal: false, currency: '',
      expired_at: null, group: 'Nezha', tags: s?.country_code || '', public_remark: s?.public_note || '',
      hidden: false, traffic_limit: 0, traffic_limit_type: 'sum', created_at: '', updated_at: iso(s?.last_active)
    };
  }

  function statusFromServer(s) {
    const h = s?.host || {}, st = s?.state || {};
    const last = new Date(s?.last_active || 0).getTime();
    return {
      client: String(s?.id ?? ''), time: iso(s?.last_active), updated_at: iso(s?.last_active),
      online: !!st && Date.now() - last < 180000, cpu: num(st.cpu), gpu: num((st.gpu || [])[0]),
      ram: num(st.mem_used), ram_total: num(h.mem_total), swap: num(st.swap_used), swap_total: num(h.swap_total),
      load: num(st.load_1), load5: num(st.load_5), load15: num(st.load_15), temp: num(st.temperatures?.[0]?.Temperature),
      disk: num(st.disk_used), disk_total: num(h.disk_total), net_in: num(st.net_in_speed), net_out: num(st.net_out_speed),
      net_total_up: num(st.net_out_transfer), net_total_down: num(st.net_in_transfer), process: num(st.process_count),
      connections: num(st.tcp_conn_count) + num(st.udp_conn_count), connections_udp: num(st.udp_conn_count),
      uptime: num(st.uptime), version: h.version || '', message: '', ping: {}
    };
  }

  function reportFromServer(s) {
    const h = s?.host || {}, st = s?.state || {};
    return {
      uuid: String(s?.id ?? ''), updated_at: iso(s?.last_active),
      cpu: { usage: num(st.cpu) }, gpu: { average_usage: num((st.gpu || [])[0]), count: (h.gpu || []).length, detailed_info: [] },
      ram: { used: num(st.mem_used), total: num(h.mem_total) }, swap: { used: num(st.swap_used), total: num(h.swap_total) },
      load: { load1: num(st.load_1), load5: num(st.load_5), load15: num(st.load_15) },
      disk: { used: num(st.disk_used), total: num(h.disk_total) },
      network: { down: num(st.net_in_speed), up: num(st.net_out_speed), totalUp: num(st.net_out_transfer), totalDown: num(st.net_in_transfer) },
      process: num(st.process_count), connections: { tcp: num(st.tcp_conn_count), udp: num(st.udp_conn_count) },
      uptime: num(st.uptime), temperature: st.temperatures || [], online: true
    };
  }

  function mergeNezhaSnapshot(data) {
    if (!data?.servers) return data;
    if (!state.snapshot?.servers) return data;
    const previous = new Map(state.snapshot.servers.map(server => [String(server.id), server]));
    return { ...state.snapshot, ...data, servers: data.servers.map(server => {
      const old = previous.get(String(server.id)) || {};
      return {
        ...old, ...server,
        host: server.host && Object.keys(server.host).length ? { ...(old.host || {}), ...server.host } : (old.host || server.host),
        state: server.state && Object.keys(server.state).length ? { ...(old.state || {}), ...server.state } : (old.state || server.state),
        public_note: server.public_note || old.public_note || ''
      };
    }) };
  }

  function acceptSnapshot(data) {
    if (!data?.servers) return;
    state.snapshot = mergeNezhaSnapshot(data);
    state.snapshotPromise = Promise.resolve(state.snapshot);
    state.listeners.forEach(fn => { try { fn(state.snapshot); } catch (_) {} });
  }

  function startNezhaStream() {
    if (!NativeWebSocket || state.streamStarted) return;
    state.streamStarted = true;
    const connect = () => {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const ws = new NativeWebSocket(`${proto}//${location.host}/api/v1/ws/server`);
      ws.onmessage = ev => { try { acceptSnapshot(JSON.parse(ev.data)); } catch (_) {} };
      ws.onclose = () => setTimeout(connect, 3000);
      ws.onerror = () => { try { ws.close(); } catch (_) {} };
    };
    connect();
  }

  async function snapshot() {
    startNezhaStream();
    if (state.snapshot) return state.snapshot;
    if (!state.snapshotPromise) state.snapshotPromise = new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Nezha server stream timeout')), 10000);
      const done = data => { clearTimeout(timer); state.listeners.delete(done); resolve(data); };
      state.listeners.add(done);
    });
    return state.snapshotPromise;
  }

  async function publicSettings() {
    if (state.settings) return state.settings;
    const data = await nativeJSON('/api/v1/setting');
    const c = data?.config || data || {};
    state.settings = {
      sitename: c.site_name || document.title || 'Nezha', description: c.description || '', custom_head: '', custom_body: '',
      oauth_enable: false, oauth_provider: '', disable_password_login: false, record_enabled: !!data?.tsdb_enabled,
      record_preserve_time: data?.tsdb_enabled ? 24 : 0, ping_record_preserve_time: data?.tsdb_enabled ? 24 : 0,
      private_site: false, visitor_audit_enabled: false, theme: cfg.short || 'nezha-komari', theme_settings: cfg.theme_settings || {}
    };
    return state.settings;
  }

  async function nodes() {
    const [snap, groupMap] = await Promise.all([snapshot(), groups()]);
    const out = {};
    (snap.servers || []).forEach(s => {
      const node = nodeFromServer(s);
      node.group = groupMap[String(s.id)] || node.group;
      out[String(s.id)] = node;
    });
    return out;
  }

  async function statuses(params = {}) {
    const snap = await snapshot(), wanted = params.uuid ? [String(params.uuid)] : (params.uuids || []).map(String), out = {};
    (snap.servers || []).forEach(s => { if (!wanted.length || wanted.includes(String(s.id))) out[String(s.id)] = statusFromServer(s); });
    return params.uuid ? out[String(params.uuid)] : out;
  }

  function periodForHours(hours) { return num(hours) > 24 ? '7d' : '1d'; }
  async function metricSeries(entity, komariMetric, hours) {
    const nezhaMetric = metricMap[komariMetric];
    if (!nezhaMetric) return { metric_key: komariMetric, entity_id: String(entity), downsampled: false, count: 0, points: [] };
    try {
      const data = await nativeJSON(`/api/v1/server/${encodeURIComponent(entity)}/metrics?metric=${encodeURIComponent(nezhaMetric)}&period=${periodForHours(hours)}`);
      const points = (data?.data_points || []).map(p => ({ time: iso(num(p.ts) < 1e12 ? num(p.ts) * 1000 : p.ts), value: num(p.value), count: 1 }));
      return { metric_key: komariMetric, entity_id: String(entity), downsampled: false, count: points.length, points };
    } catch (_) { return { metric_key: komariMetric, entity_id: String(entity), downsampled: false, count: 0, points: [] }; }
  }

  async function queryMetrics(params = {}) {
    const metrics = [...new Set([...(params.metric_keys || params.metrics || []), ...(params.metric_key ? [params.metric_key] : [])])];
    const entities = [...new Set([...(params.entity_ids || []), ...(params.entity_id ? [params.entity_id] : [])])];
    const ids = entities.length ? entities : Object.keys(await nodes());
    const jobs = ids.flatMap(id => metrics.map(metric => [id, metric]));
    const out = [];
    // Avoid hundreds/thousands of simultaneous TSDB requests on large fleets.
    for (let i = 0; i < jobs.length; i += maxMetricRequests) {
      const batch = jobs.slice(i, i + maxMetricRequests);
      out.push(...await Promise.all(batch.map(([id, metric]) => metricSeries(id, metric, params.hours || 24))));
    }
    return out;
  }

  async function recent(uuid) {
    const snap = await snapshot(), server = (snap.servers || []).find(s => String(s.id) === String(uuid));
    return server ? [reportFromServer(server)] : [];
  }

  async function records(params = {}) {
    const metricKeys = ['cpu.usage','load.average','memory.used','swap.used','disk.used','net.in.rate','net.out.rate','net.total.down','net.total.up','process.count','connections.tcp','connections.udp'];
    const series = await queryMetrics({ entity_id: params.uuid, metric_keys: metricKeys, hours: params.hours || 4 });
    const rows = new Map();
    const fields = { 'cpu.usage':'cpu','load.average':'load','memory.used':'ram','swap.used':'swap','disk.used':'disk','net.in.rate':'net_in','net.out.rate':'net_out','net.total.down':'net_total_down','net.total.up':'net_total_up','process.count':'process','connections.tcp':'connections','connections.udp':'connections_udp' };
    series.forEach(s => (s.points || []).forEach(p => { const key = p.time; const row = rows.get(key) || { client:String(params.uuid), time:key }; row[fields[s.metric_key]] = p.value; rows.set(key,row); }));
    return { records: [...rows.values()].sort((a,b) => new Date(a.time)-new Date(b.time)), count: rows.size, has_gpu_data: false };
  }

  async function rpcDispatch(call) {
    const method = call?.method || '', p = call?.params || {}, id = call?.id;
    try {
      let result;
      switch (method) {
        case 'rpc.ping': result = 'pong'; break;
        case 'rpc.getVersion': case 'common:getBackendVersion': result = { version:'nezha-komari-compat', hash:'' }; break;
        case 'rpc.getMethods': result = ['rpc.ping','rpc.getVersion','rpc.getMethods','rpc.getHelp','common:getPublicInfo','common:getNodes','common:getNodesLatestStatus','common:getNodeRecentStatus','common:getRecords']; break;
        case 'rpc.getHelp': result = { name:'Nezha Komari compatibility layer', read_only:true }; break;
        case 'common:getPublicInfo': case 'public:getPublicSettings': result = await publicSettings(); break;
        case 'common:getMe': case 'public:getMe': result = { username:'Guest', logged_in:false }; break;
        case 'common:getVersion': case 'public:getVersion': result = { version:'nezha-komari-compat', hash:'' }; break;
        case 'common:getNodes': case 'public:getNodesInformation': result = await nodes(); if (method.startsWith('public:')) result = Object.values(result); break;
        case 'common:getNodesLatestStatus': result = await statuses(p); break;
        case 'common:getNodeRecentStatus': case 'public:getClientRecentRecords': result = await recent(p.uuid); break;
        case 'common:getRecords': case 'public:getRecordsByUUID': result = await records(p); break;
        case 'public:queryMetrics': result = await queryMetrics(p); break;
        case 'public:listMetricDefinitions': result = Object.keys(metricMap).map(name => ({ name, type:'gauge', unit:'', retention_days:1 })); break;
        case 'public:getPublicPingTasks': result = []; break;
        case 'public:getPingRecords': result = { count:0, records:[], tasks:[] }; break;
        case 'public:getPingMetricStats': result = { start:iso(), end:iso(), stats:[], count:0 }; break;
        case 'public:recordVisitorEvent': result = { accepted:true }; break;
        default: return readonlyError(id);
      }
      return rpcOK(id, result);
    } catch (error) { return rpcError(id, -32000, error?.message || 'compatibility error'); }
  }

  async function rpcHTTP(request) {
    const body = JSON.parse(await request.text() || '{}');
    if (Array.isArray(body)) return response(await Promise.all(body.map(rpcDispatch)));
    return response(await rpcDispatch(body));
  }

  async function shimFetch(input, init) {
    const req = new Request(input, init), url = asURL(req), path = url.pathname;
    if (url.origin !== location.origin || !path.startsWith('/api/')) return NativeFetch(input, init);
    if (path === '/api/rpc2' && req.method === 'POST') return rpcHTTP(req);
    if (path === '/api/public') return response({ status:'success', message:'ok', data:await publicSettings() });
    if (path === '/api/me') return response({ username:'Guest', logged_in:false });
    if (path === '/api/version') return response({ version:'nezha-komari-compat', hash:'' });
    if (path === '/api/admin/theme/settings') {
      if (req.method === 'GET') return response({ status:'success', message:'ok', data:cfg.theme_settings || {} });
      return response({ status:'error', message:'Komari compatibility layer is read-only' }, 405);
    }
    if (path.startsWith('/api/recent/')) return response({ status:'success', message:'ok', data:await recent(decodeURIComponent(path.slice(12))) });
    if (path === '/api/records/load') return response(await records({ uuid:url.searchParams.get('uuid'), hours:url.searchParams.get('hours') }));
    if (path === '/api/records/ping') return response({ count:0, records:[], tasks:[] });
    if (path === '/api/nodes') return response(Object.values(await nodes()));
    if (req.method !== 'GET' && req.method !== 'HEAD') return response({ error:'read only' }, 405);
    return NativeFetch(input, init);
  }

  class KomariXMLHttpRequest extends EventTarget {
    constructor() {
      super(); this.readyState=0; this.status=0; this.statusText=''; this.responseText=''; this.response=null; this.responseType=''; this.timeout=0; this.withCredentials=false; this.upload=new EventTarget(); this.headers={};
    }
    open(method,url,async=true,user,password){ this.method=method;this.url=String(url);this.async=async!==false;this.readyState=1;this.onreadystatechange?.(new Event('readystatechange')); }
    setRequestHeader(name,value){ this.headers[name]=value; }
    getAllResponseHeaders(){ return 'content-type: application/json; charset=utf-8\r\n'; }
    getResponseHeader(name){ return String(name).toLowerCase()==='content-type'?'application/json; charset=utf-8':null; }
    abort(){ this.aborted=true;this.readyState=0;this.onabort?.(new Event('abort')); }
    send(body=null){
      if (this.aborted) return;
      const init={method:this.method||'GET',headers:this.headers,body:/^(GET|HEAD)$/i.test(this.method||'GET')?undefined:body,credentials:this.withCredentials?'include':'same-origin'};
      shimFetch(this.url,init).then(async res=>{
        if(this.aborted)return;this.status=res.status;this.statusText=res.statusText;this.responseText=await res.text();
        this.response=this.responseType==='json'?JSON.parse(this.responseText||'null'):this.responseText;this.readyState=4;
        const change=new Event('readystatechange');this.dispatchEvent(change);this.onreadystatechange?.(change);
        const event=new Event(res.ok?'load':'error');this.dispatchEvent(event);(res.ok?this.onload:this.onerror)?.(event);this.onloadend?.(new Event('loadend'));
      }).catch(error=>{if(this.aborted)return;this.status=0;this.statusText=String(error);this.readyState=4;const event=new Event('error');this.dispatchEvent(event);this.onerror?.(event);this.onloadend?.(new Event('loadend'));});
    }
  }
  Object.assign(KomariXMLHttpRequest,{UNSENT:0,OPENED:1,HEADERS_RECEIVED:2,LOADING:3,DONE:4});
  Object.assign(KomariXMLHttpRequest.prototype,{UNSENT:0,OPENED:1,HEADERS_RECEIVED:2,LOADING:3,DONE:4});

  class KomariRPCWebSocket extends EventTarget {
    static CONNECTING=0; static OPEN=1; static CLOSING=2; static CLOSED=3;
    constructor(url) {
      super(); this.url=String(url); this.readyState=0; this.protocol=''; this.extensions=''; this.bufferedAmount=0; this.binaryType='blob';
      queueMicrotask(() => { this.readyState=1; const e=new Event('open'); this.dispatchEvent(e); this.onopen?.(e); });
    }
    send(data) { Promise.resolve().then(async()=>{ const call=JSON.parse(data); const answer=await rpcDispatch(call); const e=new MessageEvent('message',{data:JSON.stringify(answer)}); this.dispatchEvent(e); this.onmessage?.(e); }); }
    close() { if(this.readyState>=2)return; this.readyState=3; const e=new CloseEvent('close',{code:1000,reason:'',wasClean:true}); this.dispatchEvent(e); this.onclose?.(e); }
    addEventListener(...args){ return super.addEventListener(...args); }
  }

  class KomariClientsWebSocket extends EventTarget {
    static CONNECTING=0; static OPEN=1; static CLOSING=2; static CLOSED=3;
    constructor(url) {
      super(); this.url=String(url); this.readyState=0; this.protocol=''; this.extensions=''; this.bufferedAmount=0; this.binaryType='blob';
      queueMicrotask(() => { this.readyState=1; const e=new Event('open'); this.dispatchEvent(e); this.onopen?.(e); });
    }
    send(command) { Promise.resolve().then(async()=>{
      const text=String(command||'').trim(), wanted=text.startsWith('get ')?text.slice(4).trim():'';
      const snap=await snapshot(), online=[], data={};
      (snap.servers||[]).forEach(server=>{const id=String(server.id);if(wanted&&id!==wanted)return;const status=statusFromServer(server);if(status.online)online.push(id);data[id]=reportFromServer(server)});
      const e=new MessageEvent('message',{data:JSON.stringify({status:'success',data:{online,data}})});this.dispatchEvent(e);this.onmessage?.(e);
    }); }
    close() { if(this.readyState>=2)return; this.readyState=3; const e=new CloseEvent('close',{code:1000,reason:'',wasClean:true}); this.dispatchEvent(e); this.onclose?.(e); }
  }

  window.fetch = shimFetch;
  window.XMLHttpRequest = KomariXMLHttpRequest;
  window.WebSocket = function(url, protocols) {
    const u = asURL(url);
    if (u.origin === location.origin && u.pathname === '/api/rpc2') return new KomariRPCWebSocket(url, protocols);
    if (u.origin === location.origin && u.pathname === '/api/clients') return new KomariClientsWebSocket(url, protocols);
    return new NativeWebSocket(url, protocols);
  };
  Object.assign(window.WebSocket, { CONNECTING:0, OPEN:1, CLOSING:2, CLOSED:3 });
  window.WebSocket.prototype = NativeWebSocket.prototype;
  window.__NEZHA_KOMARI_COMPAT_API__ = { nodeFromServer, statusFromServer, reportFromServer, rpcDispatch };
  startNezhaStream();
})();
