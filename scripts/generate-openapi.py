#!/usr/bin/env python3
"""Generate the committed HTTP contract; --check detects stale documentation in CI."""
import copy
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT = ROOT / 'internal/api/docs/openapi.json'
S = {'type': 'string'}
I = {'type': 'integer'}
B = {'type': 'boolean'}
UUID = dict(S, format='uuid')
NULL_UUID = dict(UUID, nullable=True)
LANG = dict(S, enum=['pt-BR', 'en-US', 'es-ES'])
OBJ = {'type': 'object', 'additionalProperties': True}
def obj(props, required=()):
    return {'type':'object', 'properties':props, 'required':list(required)} if required else {'type':'object','properties':props}
def ref(name): return {'$ref':'#/components/schemas/'+name}
def response(schema, description='Sucesso'):
    return {'description':description, 'content':{'application/json':{'schema':schema}}}
def param(name, schema=S, required=False, where='query', description=''):
    return {'name':name, 'in':where, 'required':required, 'schema':schema, 'description':description}
def api_schema(value):
    # Convert provider JSON Schema null unions to OpenAPI 3.0 nullable.
    if isinstance(value,list): return [api_schema(v) for v in value]
    if not isinstance(value,dict): return value
    result={k:api_schema(v) for k,v in value.items() if k not in ['$schema']}
    if isinstance(result.get('type'),list):
        kinds=result['type']
        assert 'null' in kinds and len(kinds)==2
        result['type']=next(k for k in kinds if k!='null')
        result['nullable']=True
    return result
facts=api_schema(json.loads((ROOT/'internal/api/assets/SPECIES_SCHEMA.json').read_text()))
vision=api_schema(json.loads((ROOT/'internal/api/assets/RESULT_SCHEMA.json').read_text()))
confirmation=api_schema(json.loads((ROOT/'internal/api/assets/CONFIRM_SCHEMA.json').read_text()))
analysis_fields={k:vision['properties'][k] for k in ['especie','saude','diagnostico']}
for key in ['cuidados','toxica_para_pets','temperatura','cultivo','simbolismo']:
    analysis_fields[key]=dict(facts['properties'][key], nullable=True)
    if 'enum' in analysis_fields[key]: analysis_fields[key]['enum']=analysis_fields[key]['enum']+[None]
analysis_fields['como_confirmar']=dict(confirmation['properties']['como_confirmar'],nullable=True)
analysis_fields['identification_id']=UUID
paths = {}
schemas = {
 'Analysis':obj(analysis_fields,analysis_fields.keys()),
 'SpeciesFacts':facts,
 'Error':obj({'erro':S,'cap':I,'limite':I,'motivo':S}, ['erro']),
 'OK':obj({'ok':B}, ['ok']),
 'User':obj({'id':UUID,'email':dict(S,format='email'),'email_confirmed_at':dict(S,nullable=True,format='date-time'),'user_metadata':OBJ}),
 'Session':obj({'access_token':S,'token_type':dict(S,enum=['bearer']),'expires_in':I,'expires_at':I,'user':ref('User')}, ['access_token','token_type','expires_in','expires_at','user']),
}
def op(path, method, title, tag, body=None, result=None, public=False, status='200', description='', parameters=(), example=None):
    operation = {'operationId':method+'_'+re.sub('[^a-zA-Z0-9]+','_',path).strip('_'), 'summary':title,'tags':[tag], 'description':description, 'responses':{status:response(result if result is not None else ref('OK')),'default':{'$ref':'#/components/responses/Error'}}, 'security':[] if public else [{'bearerAuth':[]}]}
    if parameters: operation['parameters']=list(parameters)
    if body is not None:
        content={'schema':body}
        if example is not None: content['example']=example
        operation['requestBody']={'required':True,'content':{'application/json':content}}
    paths.setdefault(path,{})[method]=operation
    return operation
for path in ['livez','readyz','healthz']:
    op('/'+path,'get','Liveness' if path=='livez' else 'Readiness (PostgreSQL)','Saúde', public=True)
for name in ['signup','login','recover','resend','verify']:
    fields={'email':dict(S,format='email')}
    if name in ['signup','login']: fields['password']=dict(S,format='password')
    if name=='signup': fields['name']=S
    if name=='verify': fields={'token_hash':S,'type':dict(S,enum=['email','recovery'])}
    result=ref('Session') if name in ['signup','login','verify'] else ref('OK')
    o=op('/v1/auth/'+name,'post',{'signup':'Cadastrar conta','login':'Entrar com e-mail e senha','recover':'Solicitar recuperação de senha','resend':'Reenviar confirmação','verify':'Consumir token de e-mail'}[name], 'Autenticação', obj(fields,[k for k in fields if k!='name']),result,True,description='Tokens de e-mail expiram em 1 hora. Envios compartilham intervalo por usuário; 429 inclui Retry-After. Senha nova: mínimo 8 unidades UTF-16, uma maiúscula, um caractere especial e máximo 72 bytes. Login aceita credenciais legadas.')
    if name=='signup': o['responses']['202']=response(obj({'confirmation_required':B}),'Confirmação de e-mail necessária')
for name in ['logout','refresh']:
    op('/v1/auth/'+name,'post', 'Encerrar sessão' if name=='logout' else 'Rotacionar sessão','Autenticação',result=ref('Session') if name=='refresh' else ref('OK'))
op('/v1/auth/user','get','Ler usuário da sessão','Autenticação',result=ref('User'))
op('/v1/auth/password','patch','Alterar senha e invalidar sessões anteriores','Autenticação',obj({'password':dict(S,format='password')},['password']),ref('Session'))
op('/v1/auth/google/start','post','Iniciar Google OAuth com PKCE','Google OAuth',obj({'code_challenge':dict(S,minLength=43,maxLength=43),'redirect_uri':S},['code_challenge','redirect_uri']),obj({'url':S,'state':S,'expires_in':I}),True,description='PKCE S256. Abra a URL retornada no navegador para receber o cookie. Retorno deve estar em GOOGLE_RETURN_URLS. Ver docs/google-auth.md; 503 quando desativado.')
for name in ['authorize','callback']:
    ps=[param('state',required=True)]
    if name=='callback': ps += [param('code'),param('error')]
    o=op('/v1/auth/google/'+name,'get','Redirecionamento OAuth: '+name,'Google OAuth',public=True,parameters=ps,description='Fluxo de navegação com cookie HttpOnly e estado de uso único. Abra no navegador; não use Try it out para completar login Google.')
    o['responses'].pop('200')
    o['responses']['302']={'description':'Redirecionamento ao Google ou ao retorno permitido do app','headers':{'Location':{'schema':S,'description':'URL de destino'}}}
op('/v1/auth/google/exchange','post','Trocar código temporário por sessão Broto','Google OAuth',obj({'code':S,'code_verifier':dict(S,minLength=43,maxLength=128)},['code','code_verifier']),ref('Session'),True,description='Código de uso único expira em 60 segundos; verifier original do app.')
for p,m in [('/v1/account','delete'),('/functions/v1/delete-account','post')]:
    o=op(p,m,'Excluir conta e seus dados','Conta',description='Operação destrutiva. Fotos são removidas por fila persistente; 202 indica limpeza pendente.')
    o['responses']['202']=response(obj({'ok':B,'photos_cleanup_pending':B}))
op('/v1/species-facts','get','Consultar ficha de espécie em cache','Espécies',result=dict(facts,nullable=True),parameters=[param('scientific',dict(S,maxLength=200),True),param('language',LANG,True)],description='Retorna a ficha diretamente ou null; não inicia uma análise paga.')
op('/v1/reminders/unread-count','get','Contar lembretes não lidos','Lembretes',result=obj({'count':I}))
op('/v1/reminders/read','post','Marcar todos os lembretes como lidos','Lembretes',result=obj({'ok':B,'updated':I}))
o=op('/v1/photos','post','Enviar foto binária','Fotos',status='201',result=obj({'path':S}),description='JPEG, PNG ou WebP, máximo 8 MiB. Envie bytes diretamente, não multipart/form-data. Consome quota de armazenamento.')
o['requestBody']={'required':True,'content':{t:{'schema':dict(S,format='binary')} for t in ['image/jpeg','image/png','image/webp']}}
op('/v1/photos/sign','post','Criar URL temporária para foto própria','Fotos',obj({'path':S},['path']),obj({'signedUrl':dict(S,format='uri'),'expiresIn':dict(I,enum=[300,3600])}),description='CloudFront: 300 segundos. Armazenamento local: 3600 segundos. Grave apenas path, nunca a URL assinada.')
op('/v1/photos','delete','Excluir foto sem referências','Fotos',obj({'path':S},['path']),description='409 enquanto a foto estiver vinculada a um registro.')
o=op('/v1/photos/object','get','Ler foto com assinatura local','Fotos',public=True,parameters=[param('path',required=True),param('expires',I,True),param('signature',required=True)],description='Use a URL completa retornada por /v1/photos/sign. CloudFront serve fotos em sua própria URL.')
o['responses']['200']={'description':'Imagem','content':{t:{'schema':dict(S,format='binary')} for t in ['image/jpeg','image/png','image/webp']}}
for name in ['identify','chat','search']:
    if name=='identify':
        body=obj({'photoPaths':{'type':'array','items':S,'minItems':1,'maxItems':1},'plantId':NULL_UUID,'language':dict(LANG,default='pt-BR')},['photoPaths'])
        result={'oneOf':[ref('Analysis'),ref('Error')]}
        desc='Exatamente uma foto própria. Foto ilegível retorna HTTP 200 com erro=foto_ilegivel, sem consumo. Falhas de IA não consomem créditos. Idioma ausente/desconhecido usa pt-BR.'
        example={'photoPaths':['<user-id>/<photo>.jpg'],'plantId':None,'language':'pt-BR'}
    elif name=='chat':
        body=obj({'message':dict(S,maxLength=800),'threadId':NULL_UUID,'plantId':NULL_UUID},['message'])
        result=obj({'threadId':UUID,'reply':S,'restantes':I})
        desc='Exige plano Pro válido ou assinatura de chat. Reutilize threadId. Pode consumir limite contratado.'
        example={'message':'Quando regar?','threadId':None,'plantId':None}
    else:
        body=obj({'term':S},['term'])
        result=obj({'resultados':{'type':'array','items':obj({'scientific':S,'common':S,'extract':dict(S,nullable=True),'images':{'type':'array','items':S}})},'fonte':dict(S,enum=['cache','modelo','vazio'])})
        desc='Termo menor que 3 caracteres retorna lista vazia sem campo fonte; imagens vêm da Wikimedia.'
        example={'term':'jiboia'}
    for prefix in ['/v1/','/functions/v1/']:
        op(prefix+name,'post',{'identify':'Analisar planta','chat':'Conversar sobre plantas','search':'Buscar espécies'}[name], 'IA',body,result,description=desc+(' Alias de compatibilidade.' if 'functions' in prefix else ''),example=example)
# Resource names, write allowlists and methods come from the actual router implementation.
source=(ROOT/'internal/api/data.go').read_text()
rows=re.findall(r'"(\w+)":\s*\{"([^"]*)", "([^"]*)", "([^"]*)", "([^"]*)", "([^"]*)"\}',source)
assert len(rows)==13, 'Review resources parser after changing data.go'
columns=json.loads((ROOT/'docs/data-columns.json').read_text())
def column_schema(column):
    kind=column['type']
    types={'text':S,'uuid':UUID,'boolean':B,'integer':I,'numeric':{'type':'number'},'timestamp with time zone':dict(S,format='date-time'),'date':dict(S,format='date'),'time without time zone':dict(S,example='09:00:00'),'USER-DEFINED':dict(S,enum=['free','pro'])}
    assert kind in types or kind=='jsonb', kind
    value=copy.deepcopy(types.get(kind, {}))
    if column['nullable'] and value.get('type'): value['nullable']=True
    return value
for name,key,owner,fields,methods,order in rows:
    base='/v1/data/'+name
    props={f:column_schema(columns[name][f]) for f in fields.split()}
    schemas[name+'Record']=obj({k:column_schema(v) for k,v in columns[name].items()})
    write={'type':'object','properties':props,'additionalProperties':False,'minProperties':1}
    schemas[name+'Write']=write
    desc='Somente registros autorizados para a sessão. Campos retornados preservam os nomes do banco. Não envie user_id. '
    filters=set(fields.split()+[key])
    if name in ['plant_tasks','care_events','identifications','chat_threads','reminder_events']: filters.add('plant_id')
    if name=='chat_messages': filters.add('thread_id')
    ps=[param('limit',dict(I,minimum=1,maximum=200,default=100)),param('offset',dict(I,minimum=0,default=0)),param('order',dict(S,enum=['asc','desc'],default='desc'))]
    ps += [param(f,description='Filtro de igualdade; string null testa SQL IS NULL.') for f in sorted(filters)]
    if name=='plant_tasks': ps.append(param('active',dict(S,enum=['true']),description='Exclui plantas arquivadas.'))
    op(base,'get','Ler '+name,'Dados',result=ref(name+'Record') if name=='profiles' else {'type':'array','items':ref(name+'Record')},parameters=ps,description=desc+'Ordenação por '+order+' e '+key+'. Perfil retorna objeto; demais coleções retornam lista.')
    op(base+'/{id}','get','Ler '+name+' pela chave','Dados',result=ref(name+'Record'),parameters=[param('id',S,True,'path','Chave '+key+'. Para profiles, somente ID da sessão.')],description=desc)
    for method in methods.split():
        if method=='GET': continue
        path=base if method=='POST' or name=='profiles' else base+'/{id}'
        parameters=[] if '{id}' not in path else [param('id',S,True,'path','Chave '+key+'; escape como segmento de URL.')]
        examples={'plants':{'nickname':'Minha jiboia'},'plant_groups':{'name':'Sala'},'profiles':{'display_name':'Jardineiro'},'push_tokens':{'token':'ExponentPushToken[exemplo]','platform':'android'}}
        op(path,method.lower(),method+' '+name,'Dados',dict(write, required=[f for f in fields.split() if not columns[name][f]['nullable'] and not columns[name][f]['default']]) if method=='POST' else ref(name+'Write') if method!='DELETE' else None,ref('OK') if method=='DELETE' else ref(name+'Record'),status='201' if method=='POST' else '200',parameters=parameters,description=desc+'Campos editáveis: '+(fields or 'nenhum')+'. Veja docs/api.md e migrations para tipos, obrigatoriedade e regras de negócio.',example=examples.get(name))
    if name=='profiles':
        paths[base+'/{id}']['patch']=copy.deepcopy(paths[base]['patch'])
        paths[base+'/{id}']['patch']['operationId']+='ById'
        paths[base+'/{id}']['patch']['parameters']=[param('id',UUID,True,'path','ID da sessão.')]
spec={'openapi':'3.0.3','info':{'title':'Broto API','version':'1.0.0','description':'Contrato HTTP do backend Go. Sessões opacas via Bearer, não JWT Supabase. Try it out executa operações reais, incluindo exclusões e IA. Integrações dependem da configuração do servidor. Veja docs/api.md e docs/google-auth.md.'},'servers':[{'url':'/','description':'Mesmo servidor da documentação'}], 'tags':[{'name':n} for n in ['Saúde','Autenticação','Google OAuth','Conta','Espécies','Lembretes','Fotos','IA','Dados']], 'security':[{'bearerAuth':[]}], 'paths':paths,'components':{'securitySchemes':{'bearerAuth':{'type':'http','scheme':'bearer','description':'Cole somente o access_token de uma sessão Broto. Não use chaves de provedores.'}},'schemas':schemas,'responses':{'Error':{'description':'Erro JSON: 400 entrada inválida; 401 sessão; 402 créditos/plano; 403 acesso/origem; 404 inexistente; 409 conflito; 429 limite; 500 erro interno; 502 provedor de IA; 503 integração/banco/concorrência; 507 quota de fotos. Consulte erro para o código específico.','headers':{'Retry-After':{'description':'Segundos até nova tentativa quando aplicável','schema':S}},'content':{'application/json':{'schema':ref('Error'),'example':{'erro':'sem_token'}}}}}}}
def remove_empty_required(value):
    if isinstance(value,dict):
        if value.get('required') == []: del value['required']
        for child in value.values(): remove_empty_required(child)
    elif isinstance(value,list):
        for child in value: remove_empty_required(child)
remove_empty_required(spec)
output=json.dumps(spec,ensure_ascii=False,indent=2)+'\n'
if '--check' in sys.argv:
    if not OUT.exists() or OUT.read_text()!=output:
        sys.exit('OpenAPI desatualizado: execute python3 scripts/generate-openapi.py')
else:
    OUT.write_text(output)
print(f'OpenAPI: {len(paths)} caminhos, {sum(len(v) for v in paths.values())} operações')
