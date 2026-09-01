// 迷你 webpack 运行时 + pb 类反射。
// 加载 build/modules.js，require agent_service_pb 等模块，
// 从 AgentService 的 methods 出发 BFS 所有消息类，输出 FileDescriptorSet JSON。
const path = require('path');
const modules = require(path.join(__dirname, '..', 'build', 'modules.js'));
const fs = require('fs');

const cache = {};
function req(id) {
  if (id.startsWith('node:')) return require(id);
  if (!(id in modules)) {
    // node builtin shim 模块（bundle 内 e.exports=require("assert") 形式）已在 map 里
    throw new Error('unknown module: ' + id);
  }
  if (!(id in cache)) {
    const m = { exports: {} };
    cache[id] = m.exports;
    modules[id](m, m.exports, req);
    cache[id] = m.exports;
  }
  return cache[id];
}

// webpack require helpers
req.d = (exports, definition) => {
  for (const key in definition) {
    if (!Object.prototype.hasOwnProperty.call(exports, key)) {
      Object.defineProperty(exports, key, { enumerable: true, get: definition[key] });
    }
  }
};
req.o = (obj, prop) => Object.prototype.hasOwnProperty.call(obj, prop);
req.r = (exports) => {
  if (typeof Symbol !== 'undefined' && Symbol.toStringTag) {
    Object.defineProperty(exports, Symbol.toStringTag, { value: 'Module' });
  }
  Object.defineProperty(exports, '__esModule', { value: true });
};
req.n = (module) => {
  const getter = module && module.__esModule ? () => module.default : () => module;
  req.d(getter, { a: getter });
  return getter;
};
req.m = (module) => module;
req.t = function (value, mode) {
  if (mode & 1) value = req(value);
  if (mode & 8) return value;
  if (typeof value === 'object' && value !== null) {
    if ((mode & 4) && value.__esModule) return value;
    if ((mode & 16) && typeof value.then === 'function') return value;
  }
  const ns = Object.create(null);
  req.r(ns);
  const def = {};
  if (mode & 2 && value && typeof value === 'object') {
    for (const key of Object.keys(value)) def[key] = () => value[key];
  }
  def['default'] = () => value;
  req.d(ns, def);
  return ns;
};
req.g = globalThis;
req.c = cache;

// 找模块 id
const ids = Object.keys(modules);

const svcModId = ids.find((k) => modules[k].toString().includes('typeName:"agent.v1.AgentService"'));
if (!svcModId) throw new Error('AgentService definition not found');
console.error('service module:', svcModId);

// 静态解析服务定义：methods 表里的 I/O 是 "别名.导出名" 形式，
// 别名在模块顶部 var 别名=n("模块id") 定义。
const svcSrc = modules[svcModId].toString();
const aliasToMod = {};
for (const m of svcSrc.matchAll(/(\w+)=n\("([^"]+)"\)/g)) {
  aliasToMod[m[1]] = m[2];
}
const resolveRef = (alias, exp) => {
  const modId = aliasToMod[alias];
  if (!modId) throw new Error(`unknown alias ${alias}`);
  const mod = req(modId);
  const v = mod[exp];
  if (!v || !v.typeName) throw new Error(`bad ref ${alias}.${exp} in ${modId}`);
  return v;
};

const KindMap = { Unary: 0, ServerStreaming: 1, ClientStreaming: 2, BiDiStreaming: 3 };
const svcDefMatch = svcSrc.match(/typeName:"agent\.v1\.AgentService",methods:\{/);
if (!svcDefMatch) throw new Error('AgentService def parse failed');
const svcTail = svcSrc.slice(svcDefMatch.index);
const agentService = { typeName: 'agent.v1.AgentService', methods: {} };
for (const m of svcTail.matchAll(/(\w+):\{name:"(\w+)",I:([\w$]+)\.([\w$]+),O:([\w$]+)\.([\w$]+),kind:\w+\.I\.(\w+)\}/g)) {
  agentService.methods[m[1]] = {
    name: m[2],
    kind: KindMap[m[7]],
    I: resolveRef(m[3], m[4]),
    O: resolveRef(m[5], m[6]),
  };
}
if (Object.keys(agentService.methods).length === 0) throw new Error('no methods parsed');

const methodRoots = [];
for (const [mkey, m] of Object.entries(agentService.methods)) {
  methodRoots.push({ method: m.name, kind: m.kind, input: m.I && m.I.typeName, output: m.O && m.O.typeName });
}
console.error(JSON.stringify(methodRoots, null, 1));

// ---- BFS 消息闭包 ----
const ScalarType = { DOUBLE:1, FLOAT:2, INT64:3, UINT64:4, INT32:5, FIXED64:6, FIXED32:7, BOOL:8, STRING:9, GROUP:10, MESSAGE:11, BYTES:12, UINT32:13, ENUM:14, SFIXED32:15, SFIXED64:16, SINT32:17, SINT64:18 };

const seen = new Map(); // typeName -> class
const queue = [];
const enumsSeen = new Map();

function addMsg(cls) {
  if (!cls || !cls.typeName) return;
  if (seen.has(cls.typeName)) return;
  seen.set(cls.typeName, cls);
  queue.push(cls);
}

for (const m of Object.values(agentService.methods)) {
  addMsg(m.I);
  addMsg(m.O);
}

const googleDeps = new Set();

function fieldToProto(f) {
  // f: protobuf-es v1 FieldInfo {no,name,localName,kind,scalar,T,repeated,opt,oneof,packed,delimited?}
  const out = { name: f.name, number: f.no, jsonName: f.localName };
  out.label = f.repeated ? 3 : 1; // LABEL_REPEATED / LABEL_OPTIONAL
  if (f.kind === 'scalar') {
    out.type = f.T; // protobuf-es v1: scalar 字段的 T 即 ScalarType
  } else if (f.kind === 'message') {
    out.type = 11;
    const cls = f.T; // message 字段的 T 即消息类（含 typeName）
    const tn = cls.typeName;
    if (tn.startsWith('google.protobuf.')) {
      // 依赖经 googleFileFor(tn) 记录（按消息推导所属 .proto），
      // 此前这里算过一个 file 变量但从未使用——删掉以免误导
      out.typeName = '.' + tn;
      googleDeps.add(googleFileFor(tn));
    } else {
      addMsg(cls);
      out.typeName = '.' + mangleRef(tn);
    }
  } else if (f.kind === 'enum') {
    // 线上格式 enum==int32 varint，简化为 int32，避免枚举类型重建
    out.type = 5;
  } else if (f.kind === 'map') {
    out.type = 11;
    const entryName = f.name.replace(/(^|_)(\w)/g, (_, p, c) => c.toUpperCase()) + 'Entry';
    out.mapEntry = {
      name: entryName,
      key: f.K, // scalar
      valueKind: f.V.kind,
      valueScalar: f.V.kind === 'scalar' ? (f.V.T !== undefined ? f.V.T : f.V.scalar) : null,
      valueMsg: f.V.kind === 'message' ? (f.V.T.typeName || null) : null,
    };
    if (f.V.kind === 'message' && f.V.T.typeName && !f.V.T.typeName.startsWith('google.protobuf.')) {
      addMsg(f.V.T);
    }
    if (f.V.kind === 'message' && f.V.T.typeName && f.V.T.typeName.startsWith('google.protobuf.')) {
      googleDeps.add(googleFileFor(f.V.T.typeName));
      out.mapEntry.valueMsgGoogle = '.' + f.V.T.typeName;
    }
  }
  if (f.oneof) out.oneof = typeof f.oneof === 'string' ? f.oneof : (f.oneof.localName || f.oneof.name);
  if (f.opt) out.proto3Optional = true;
  if (f.packed) out.packed = true;
  return out;
}

function googleFileFor(tn) {
  const map = {
    'google.protobuf.Struct': 'google/protobuf/struct.proto',
    'google.protobuf.Value': 'google/protobuf/struct.proto',
    'google.protobuf.ListValue': 'google/protobuf/struct.proto',
    'google.protobuf.Timestamp': 'google/protobuf/timestamp.proto',
    'google.protobuf.Duration': 'google/protobuf/duration.proto',
    'google.protobuf.Any': 'google/protobuf/any.proto',
    'google.protobuf.Empty': 'google/protobuf/empty.proto',
    'google.protobuf.StringValue': 'google/protobuf/wrappers.proto',
    'google.protobuf.Int32Value': 'google/protobuf/wrappers.proto',
    'google.protobuf.Int64Value': 'google/protobuf/wrappers.proto',
    'google.protobuf.BoolValue': 'google/protobuf/wrappers.proto',
    'google.protobuf.UInt32Value': 'google/protobuf/wrappers.proto',
    'google.protobuf.UInt64Value': 'google/protobuf/wrappers.proto',
    'google.protobuf.DoubleValue': 'google/protobuf/wrappers.proto',
    'google.protobuf.FloatValue': 'google/protobuf/wrappers.proto',
    'google.protobuf.BytesValue': 'google/protobuf/wrappers.proto',
  };
  const f = map[tn];
  if (!f) throw new Error('unknown google type: ' + tn);
  return f;
}

// typeName -> descriptor 内引用名（嵌套类型拍平，点换下划线）
function mangleName(tn, pkg) {
  const local = tn.slice(pkg.length + 1);
  return local.replace(/\./g, '_');
}
function mangleRef(tn) {
  const pkg = tn.split('.').slice(0, 2).join('.'); // 假设都是 xxx.v1 形式
  return tn.slice(0, pkg.length + 1) + mangleName(tn, pkg);
}

while (queue.length) {
  const cls = queue.shift();
  const fields = cls.fields.list();
  cls._extractedFields = fields.map(fieldToProto);
}

console.error('messages:', seen.size);

// 按 package 分组生成 FileDescriptorProto
const byPkg = new Map();
for (const [tn, cls] of seen) {
  if (tn.startsWith('google.protobuf.')) continue;
  const pkg = tn.split('.').slice(0, 2).join('.');
  if (!byPkg.has(pkg)) byPkg.set(pkg, []);
  byPkg.get(pkg).push([tn, cls]);
}

const files = [];
for (const [pkg, msgs] of byPkg) {
  const fd = {
    name: `cursor/${pkg.replace(/\./g, '/')}/bundle.proto`,
    package: pkg,
    syntax: 'proto3',
    dependency: [...googleDeps],
    messageType: [],
    service: pkg === 'agent.v1' ? [{
      name: 'AgentService',
      method: Object.values(agentService.methods).map((m) => ({
        name: m.name,
        inputType: '.' + mangleRef(m.I.typeName),
        outputType: '.' + mangleRef(m.O.typeName),
        clientStreaming: m.kind === 3 || m.kind === 2, // BiDi/ClientStreaming
        serverStreaming: m.kind === 3 || m.kind === 1, // BiDi/ServerStreaming
      })),
    }] : [],
  };
  for (const [tn, cls] of msgs) {
    const md = { name: mangleName(tn, pkg), field: [], oneofDecl: [], nestedType: [] };
    const oneofIdx = new Map();
    // 第一遍：真实 oneof 必须排在 synthetic (proto3_optional) oneof 之前
    for (const f of cls._extractedFields) {
      if (f.oneof && !f.proto3Optional && !oneofIdx.has(f.oneof)) {
        oneofIdx.set(f.oneof, md.oneofDecl.length);
        md.oneofDecl.push({ name: f.oneof });
      }
    }
    for (const f of cls._extractedFields) {
      if (f.mapEntry) {
        const entry = {
          name: f.mapEntry.name,
          options: { mapEntry: true },
          field: [
            { name: 'key', number: 1, label: 1, type: f.mapEntry.key, jsonName: 'key' },
            f.mapEntry.valueKind === 'message'
              ? { name: 'value', number: 2, label: 1, type: 11, typeName: f.mapEntry.valueMsgGoogle || ('.' + mangleRef(f.mapEntry.valueMsg)), jsonName: 'value' }
              : f.mapEntry.valueKind === 'enum'
                ? { name: 'value', number: 2, label: 1, type: 5, jsonName: 'value' }
                : { name: 'value', number: 2, label: 1, type: f.mapEntry.valueScalar, jsonName: 'value' },
          ],
        };
        md.nestedType.push(entry);
        md.field.push({
          name: f.name, number: f.number, label: 3, type: 11,
          typeName: `.${pkg}.${md.name}.${f.mapEntry.name}`, jsonName: f.jsonName,
        });
        continue;
      }
      const fd2 = {
        name: f.name, number: f.number, label: f.label, jsonName: f.jsonName,
      };
      if (f.type) fd2.type = f.type;
      if (f.typeName) fd2.typeName = f.typeName;
      if (f.oneof) {
        if (!oneofIdx.has(f.oneof)) {
          oneofIdx.set(f.oneof, md.oneofDecl.length);
          md.oneofDecl.push({ name: f.oneof });
        }
        fd2.oneofIndex = oneofIdx.get(f.oneof);
      }
      if (f.proto3Optional) {
        // proto3_optional 需要 synthetic oneof
        const oname = '_' + f.name;
        oneofIdx.set(oname, md.oneofDecl.length);
        md.oneofDecl.push({ name: oname });
        fd2.oneofIndex = md.oneofDecl.length - 1;
        fd2.proto3Optional = true;
      }
      md.field.push(fd2);
    }
    fd.messageType.push(md);
  }
  files.push(fd);
}

// 后处理：跨 package 引用补 dependency
{
  const pkgToFile = new Map(files.map((f) => [f.package, f.name]));
  const collectTypeNames = (obj, acc) => {
    if (Array.isArray(obj)) { obj.forEach((x) => collectTypeNames(x, acc)); return; }
    if (obj && typeof obj === 'object') {
      if (typeof obj.typeName === 'string') acc.push(obj.typeName);
      Object.values(obj).forEach((x) => collectTypeNames(x, acc));
    }
  };
  for (const f of files) {
    const refs = [];
    collectTypeNames(f.messageType, refs);
    collectTypeNames(f.service || [], refs);
    for (const ref of refs) {
      // 只关心 "点+包路径+类型名" 形态；match 结果仅作形态过滤
      if (!/^\.([a-z0-9_.]+)\.[^.]+$/.test(ref)) continue;
      // 包名 = 去掉最后一个段后的前缀中匹配已知包的最长者
      for (const [pkg, fileName] of pkgToFile) {
        if (ref.startsWith('.' + pkg + '.') && pkg !== f.package && !f.dependency.includes(fileName)) {
          f.dependency.push(fileName);
        }
      }
    }
  }
}

fs.mkdirSync('schema', { recursive: true });
fs.writeFileSync('schema/cursor_fds.json', JSON.stringify({ file: files }, null, 1));
console.error('wrote schema/cursor_fds.json, files:', files.map((f) => f.name + ' (' + f.messageType.length + ' msgs)').join(', '));
