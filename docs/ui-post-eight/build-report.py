"""Validate immutable Council scores and export exact arithmetic as CSV/XLSX."""
from pathlib import Path
from fractions import Fraction
from decimal import Decimal, ROUND_HALF_UP
from collections import defaultdict
from xml.sax.saxutils import escape
import csv, hashlib, json, re, struct, zipfile
import xml.etree.ElementTree as ET
P=Path(__file__).resolve().parent
WEIGHTS=dict(hierarchy='.20',typography='.15',density_layout='.20',color='.15',visible_affordances='.15',coherence='.15')
BRAND=['palette_tokens','type_geometry','shell','accent_discipline','suite_cohesion']
def rounded(n):return str((Decimal(n.numerator)/Decimal(n.denominator)).quantize(Decimal('.1'),rounding=ROUND_HALF_UP))
inventories=[]
for path in sorted(P.glob('inventory-v*.json')):
 rows=json.loads(path.read_text());pins={r['file']:r for r in rows};assert len(rows)==len(pins)
 for r in rows:
  data=(P/r['file']).read_bytes();assert hashlib.sha256(data).hexdigest()==r['sha256'] and len(data)==r['bytes'];assert struct.unpack('>II',data[16:24])==(r['width'],r['height'])
 inventories.append(pins)
scores=[['Round','Reviewer','File','Cohort',*WEIGHTS,'Quality','Brand exact','Report SHA256']]
cohorts=[['Round','Reviewer','Cohort','n','Sum','Exact mean','Display mean','Satisfied']]
spreads=[['Round','File','Dimension','Claude','Sol','Spread']]
progression=[['Round','Claude exact','Sol exact','Combined exact','Rounded','Accepted']]
reports={}
for path in sorted(P.glob('*-round*.json')):
 match=re.fullmatch(r'(opus|sol|gemini)-round(\d+)\.json',path.name)
 if not match:continue
 who,num=match[1],int(match[2]);r=json.loads(path.read_text());assert r['schema']=='ky-ui-council-scores/2' and r['rubric']['version']=='ky-suite-v2';assert {k:Fraction(str(v))for k,v in r['rubric']['weights'].items()}=={k:Fraction(v)for k,v in WEIGHTS.items()}
 files={f['file']:f for f in r['files']};pins=next(i for i in inventories if i.keys()==files.keys());assert len(files)==len(r['files']);groups=defaultdict(list)
 for name,f in files.items():
  assert all(f[k]==pins[name][k] for k in ['sha256','width','height']);assert all(type(f['scores'][k])is int and 0<=f['scores'][k]<=10 for k in WEIGHTS)
  total=Fraction(rounded(sum(Fraction(v)*f['scores'][k]for k,v in WEIGHTS.items())));assert total==Fraction(str(f['total']))
  values=[f['ky_fit']['scores'][k]for k in BRAND if f['ky_fit']['scores'][k]not in ['N/A',None]];assert values and all(type(v)is int and 0<=v<=10 for v in values);brand=sum(map(Fraction,values))/len(values)
  if not f.get('duplicate_of'):groups[f['cohort']].append(total)
  scores.append([num,who,name,f['cohort'],*[f['scores'][k]for k in WEIGHTS],str(total),str(brand),hashlib.sha256(path.read_bytes()).hexdigest()])
 satisfied=r.get('verdict',{}).get('satisfied');means={c:sum(v)/len(v)for c,v in groups.items()}
 for c,v in groups.items():cohorts.append([num,who,c,len(v),str(sum(v)),str(means[c]),rounded(means[c]),satisfied])
 quality=[Fraction(str(f['total']))for f in files.values()if not f.get('duplicate_of')];reports[who,num]={'files':files,'mean':sum(quality)/len(quality),'cohorts':means,'satisfied':satisfied}
numbers={n for _,n in reports}
if (P/'manifest-03-after.json').exists():numbers.add(2)
for num in sorted(numbers):
 a,b=reports.get(('opus',num)),reports.get(('sol',num))
 if a and b:
  assert a['files'].keys()==b['files'].keys();mean=(a['mean']+b['mean'])/2;progression.append([num,str(a['mean']),str(b['mean']),str(mean),rounded(mean),Decimal(rounded(mean))>=8])
  for name,x in a['files'].items():
   for k,v in x['scores'].items():y=b['files'][name]['scores'][k];spreads.append([num,name,k,v,y,abs(v-y)])
 if not(a and b):progression.append([num,str(a['mean'])if a else 'Pending',str(b['mean'])if b else 'Pending','Pending','Pending','Pending'])
notes=[['Topic','Evidence'],['Scope','32 initial-view captures across eight everyday-user surfaces; no competitor/whole-product acceptance.'],['Conditions','Read immutable manifests; desktop1280x900/phone390x844,OSlight/dark,DPR1,normal service workers.'],['Login artifact','R1 preserves four mid-fade login shots; stage01 settled before-shots are supplemental comparison evidence and carry no replacement score.'],['Progression','Same-set Claude/Sol quality mean, half-up to one decimal; >=8.0 accepts. Brand is separate; raw scores/satisfaction retained.'],['Gemini','Unused until both primary reviewers genuinely satisfied; advisory, audited, never delaying accepted progression.']]
for f in sorted(P.glob('round*-audit.json')):
 audit=json.loads(f.read_text())
 for n,h in audit.get('raw_reports',{}).items():assert hashlib.sha256((P/n).read_bytes()).hexdigest()==h
 notes.append([f.name,json.dumps(audit)])
sheets={'Scores':scores,'Cohorts':cohorts,'Progression':progression,'Spreads':spreads,'Notes':notes}
for name,rows in sheets.items():
 with (P/(name.lower()+'.csv')).open('w',newline='')as f:csv.writer(f,lineterminator='\n').writerows(rows)
lines=['# KyPost Council score history','','Coverage summaries are not rankings. Raw originals and independent satisfaction remain unchanged.','','| Round | Claude | Sol | Combined exact | Rounded progression | Accepted | Gemini |','| --- | ---: | ---: | ---: | ---: | --- | --- |']
for row in progression[1:]:lines.append('| '+' | '.join(map(str,row))+' | Unused |')
lines+=['','R1 includes the original four mid-animation login captures. Supplemental settled login before-views are unscored; do not call a capture correction product improvement.','','| Round | Reviewer | Cohort | n | Sum | Exact mean | Display | Satisfied |','| --- | --- | --- | ---: | ---: | ---: | ---: | --- |']
for row in cohorts[1:]:lines.append('| '+' | '.join(map(str,row))+' |')
(P/'COUNCIL.md').write_text('\n'.join(lines)+'\n')
def col(n):
 s=''
 while n:n,r=divmod(n-1,26);s=chr(65+r)+s
 return s
ns='http://schemas.openxmlformats.org/spreadsheetml/2006/main'
with zipfile.ZipFile(P/'KyPost-Council.xlsx','w',zipfile.ZIP_DEFLATED)as z:
 z.writestr('[Content_Types].xml','<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>'+''.join(f'<Override PartName="/xl/worksheets/sheet{i}.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>'for i in range(1,len(sheets)+1))+'</Types>')
 z.writestr('_rels/.rels','<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>')
 z.writestr('xl/workbook.xml',f'<workbook xmlns="{ns}" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>'+''.join(f'<sheet name="{name}" sheetId="{i}" r:id="rId{i}"/>'for i,name in enumerate(sheets,1))+'</sheets></workbook>')
 z.writestr('xl/_rels/workbook.xml.rels','<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'+''.join(f'<Relationship Id="rId{i}" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet{i}.xml"/>'for i in range(1,len(sheets)+1))+'</Relationships>')
 for i,rows in enumerate(sheets.values(),1):
  xml=f'<worksheet xmlns="{ns}"><sheetData>'+''.join(f'<row r="{j}">'+''.join(f'<c r="{col(k)}{j}" t="inlineStr"><is><t xml:space="preserve">{escape(str(v))}</t></is></c>'for k,v in enumerate(row,1))+'</row>'for j,row in enumerate(rows,1))+'</sheetData></worksheet>';ET.fromstring(xml);z.writestr(f'xl/worksheets/sheet{i}.xml',xml)
with zipfile.ZipFile(P/'KyPost-Council.xlsx')as z:
 assert z.testzip()is None
 for name in z.namelist():
  if name.endswith('.xml')or name.endswith('.rels'):ET.fromstring(z.read(name))
print('Validated originals, raw scores, exact progression and XLSX XML')
