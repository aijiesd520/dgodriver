"""Import a zendriver PyCDP snapshot without importing or executing its code.

Development-only: python tools/import_protocol.py ../zendriver-main/zendriver/cdp
Runtime and normal regeneration (go generate ./cdp) require only Go.
"""
import ast
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
sources = {p.stem: (p.read_text(encoding="utf-8"), ast.parse(p.read_text(encoding="utf-8"))) for p in sorted(root.glob("*.py")) if p.stem not in {"__init__", "util"}}
domains = {k: re.search(r"# CDP domain: (\w+)", v[0])[1] for k, v in sources.items()}

def camel(s):
    acronyms = {"id": "ID", "ids":"IDs", "dom":"DOM", "guid":"GUID", "url": "URL", "uri": "URI", "html": "HTML", "xml": "XML", "css": "CSS", "js": "JS", "api": "API", "uuid": "UUID", "http": "HTTP", "https": "HTTPS", "dns": "DNS", "ssl": "SSL", "ip": "IP", "ax": "AX"}
    if s.isupper(): s=s.lower()
    if "_" in s:
        return "".join(acronyms.get(p.lower(), p[:1].upper()+p[1:]) for p in s.strip("_").split("_") if p)
    parts=re.findall(r"[A-Z]+(?=[A-Z][a-z]|$)|[A-Z]?[a-z]+|[0-9]+",s)
    return "".join(acronyms.get(p.lower(),p[:1].upper()+p[1:]) for p in parts)

def typename(n, domain):
    if isinstance(n, ast.Constant):
        if n.value is None: return "any"
        return typename(ast.parse(n.value, mode="eval").body, domain)
    if isinstance(n, ast.Name):
        return {"str":"string", "int":"int", "float":"float64", "bool":"bool", "dict":"map[string]any", "list":"[]any", "Any":"any", "T_JSON_DICT":"map[string]any"}.get(n.id, domain+camel(n.id))
    if isinstance(n, ast.Attribute):
        if isinstance(n.value, ast.Name) and n.value.id in domains:
            return domains[n.value.id]+camel(n.attr)
        return typename(ast.Name(id=n.attr), domain)
    if isinstance(n, ast.Subscript):
        kind = ast.unparse(n.value).split(".")[-1]
        sub = n.slice
        if kind == "Optional":
            t=typename(sub,domain)
            return t if t.startswith(("[]","map[")) or t=="any" else "*"+t
        if kind in {"List","list","Sequence"}: return "[]"+typename(sub,domain)
        if kind in {"Dict","dict"}: return "map["+typename(sub.elts[0],domain)+"]"+typename(sub.elts[1],domain)
        if kind == "Union": return "any"
    raise ValueError("Unknown annotation: "+ast.unparse(n))

def mapping(node, container):
    result={}
    for x in ast.walk(node):
        if container == "json" and isinstance(x, ast.keyword):
            key=next((v.slice.value for v in ast.walk(x.value) if isinstance(v,ast.Subscript) and isinstance(v.value,ast.Name) and v.value.id=="json" and isinstance(v.slice,ast.Constant)),None)
            if key is not None: result[x.arg]=key
        if isinstance(x,ast.Assign):
            for target in x.targets:
                if isinstance(target,ast.Subscript) and isinstance(target.value,ast.Name) and target.value.id==container and isinstance(target.slice,ast.Constant):
                    names=[v.attr if isinstance(v,ast.Attribute) and isinstance(v.value,ast.Name) and v.value.id=="self" else v.id for v in ast.walk(x.value) if (isinstance(v,ast.Attribute) and isinstance(v.value,ast.Name) and v.value.id=="self") or isinstance(v,ast.Name)]
                    for name in names: result.setdefault(name,target.slice.value)
    return result

def field(name, annotation, key, domain):
    t=typename(annotation,domain)
    optional=isinstance(annotation,ast.Subscript) and ast.unparse(annotation.value).endswith("Optional")
    return {"Name":camel(name),"Type":t,"JSON":key,"Optional":optional}

output=[]; count=[0,0,0]
for module,(src,tree) in sources.items():
    domain=domains[module]; types=[];commands=[]
    for n in tree.body:
        if isinstance(n,ast.ClassDef):
            name=domain+camel(n.name);attrs=[x for x in n.body if isinstance(x,ast.AnnAssign)]
            event=next((x.args[0].value for x in n.decorator_list if isinstance(x,ast.Call) and ast.unparse(x.func)=="event_class"),"")
            if attrs:
                keys=mapping(n,"json")
                fs=[field(x.target.id,x.annotation,keys[x.target.id],domain) for x in attrs]
                types.append({"Name":name,"Kind":"struct","Fields":fs,"Event":event})
            elif any(ast.unparse(b)=="enum.Enum" for b in n.bases):
                vals={name+camel(x.targets[0].id):x.value.value for x in n.body if isinstance(x,ast.Assign) and isinstance(x.value,ast.Constant)}
                types.append({"Name":name,"Kind":"string","Values":vals})
            elif n.bases:
                base=ast.unparse(n.bases[0]);kind={"str":"string","int":"int","float":"float64","dict":"map[string]any"}.get(base)
                if kind is None:
                    method=next(x for x in n.body if isinstance(x,ast.FunctionDef) and x.name=="to_json")
                    kind=typename(method.returns,domain)
                types.append({"Name":name,"Kind":kind})
            else:
                types.append({"Name":name,"Kind":"struct","Fields":[],"Event":event})
            count[0]+=1
            if event: count[2]+=1
        elif isinstance(n,ast.FunctionDef):
            method=next((x.value for x in ast.walk(n) if isinstance(x,ast.Constant) and isinstance(x.value,str) and re.fullmatch(re.escape(domain)+r"\.\w+",x.value)),None)
            if method is None: continue
            keys=mapping(n,"params")
            fs=[field(a.arg,a.annotation,keys[a.arg],domain) for a in n.args.args]
            ret=n.returns.slice.elts[-1]
            ret_types=ret.slice.elts if isinstance(ret,ast.Subscript) and ast.unparse(ret.value).endswith("Tuple") else ([] if isinstance(ret,ast.Constant) and ret.value is None else [ret])
            returns=next((x.value for x in ast.walk(n) if isinstance(x,ast.Return)),None)
            values=returns.elts if isinstance(returns,ast.Tuple) else ([] if not ret_types else [returns])
            rf=[]
            for typ,expr in zip(ret_types,values):
                key=next(x.slice.value for x in ast.walk(expr) if isinstance(x,ast.Subscript) and isinstance(x.value,ast.Name) and x.value.id=="json" and isinstance(x.slice,ast.Constant))
                rf.append(field(key,typ,key,domain))
            assert len(rf)==len(ret_types),method
            commands.append({"Name":camel(method.split(".")[1]),"Method":method,"Params":fs,"Result":rf});count[1]+=1
    output.append({"Name":domain,"File":module,"Types":types,"Commands":commands})
dest=pathlib.Path(__file__).resolve().parents[1]/"cdp"/"protocol.json"
dest.write_text(json.dumps({"Version":"0.17.1","Domains":output},ensure_ascii=False,indent=2)+"\n",encoding="utf-8")
print(f"Imported {len(output)} domains, {count[0]} types, {count[1]} commands, {count[2]} events")
