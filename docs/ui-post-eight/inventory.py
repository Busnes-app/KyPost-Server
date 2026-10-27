"""Verify staged originals and measure every same-size duplicate comparison."""
from pathlib import Path
from PIL import Image
import hashlib,itertools,json,sys,numpy as np
P=Path(__file__).resolve().parent
manifest,version=sys.argv[1:];m=json.loads((P/manifest).read_text());rows=sorted(m['shots'],key=lambda r:r['file']);assert len(rows)==32
arrays={};pairs=[]
for r in rows:
 data=(P/r['file']).read_bytes();assert hashlib.sha256(data).hexdigest()==r['sha256'] and len(data)==r['bytes'];im=Image.open(P/r['file']).convert('RGB');assert im.size==(r['width'],r['height']);arrays[r['file']]=np.asarray(im).astype(np.int16)
for a,b in itertools.combinations(rows,2):
 if (a['width'],a['height'])!=(b['width'],b['height']):continue
 d=np.abs(arrays[a['file']]-arrays[b['file']]);fraction=float(np.count_nonzero(np.any(d,axis=2))/d.shape[0]/d.shape[1]);mx=int(d.max());near=fraction<.001 and mx<=8;pairs.append({'a':a['file'],'b':b['file'],'changed_fraction':fraction,'max_channel_delta':mx,'near_duplicate':near})
 if near and not a.get('duplicate_of'):b['duplicate_of']=a['file']
assert len(pairs)==240
(P/f'inventory-{version}.json').write_text(json.dumps(rows,indent=2)+'\n');(P/f'duplicate-audit-{version}.json').write_text(json.dumps({'originals':len(rows),'same_size_pairs':len(pairs),'exact_unique':len({r['sha256']for r in rows}),'near_duplicates':sum(r['near_duplicate']for r in pairs),'pairs':pairs},indent=2)+'\n')
print(version,':32 originals,240 same-size pairs,',sum(r['near_duplicate']for r in pairs),'near duplicates')
