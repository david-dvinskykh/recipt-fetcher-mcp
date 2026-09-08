package authweb

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/browserlogin"
)

// providerCard is what the login page shows per store.
type providerCard struct {
	ID        string
	Name      string
	LoggedIn  bool
	Guided    bool // has a browser-driven login
	PasteForm browserlogin.Form
}

// mailPaste is the IMAP mailbox login form (shared fallback source).
func mailPaste() browserlogin.Driver {
	return &pasteFormDriver{provider: "mail", form: browserlogin.Form{
		Title: "Connect the mailbox (Action/Allegro e-mail fallback)",
		Note:  "For Gmail use an app password, not the account password.",
		Fields: []browserlogin.Field{
			{Name: "host", Label: "IMAP host", Type: "text", Placeholder: "imap.gmail.com", Required: true},
			{Name: "port", Label: "Port", Type: "text", Placeholder: "993"},
			{Name: "username", Label: "Username", Type: "text", Required: true},
			{Name: "password", Label: "Password / app password", Type: "password", Required: true},
			{Name: "mailbox", Label: "Folder", Type: "text", Placeholder: "INBOX"},
		},
	}}
}

// pasteFormDriver is a trivial single-form driver used for mail; the store-side
// PasteDriver types cover the shops.
type pasteFormDriver struct {
	provider string
	form     browserlogin.Form
}

func (d *pasteFormDriver) Provider() string { return d.provider }
func (d *pasteFormDriver) Run(ctx context.Context, ask browserlogin.Prompt) (map[string]string, string, error) {
	v, err := ask(d.form)
	if err != nil {
		return nil, "", err
	}
	return v, "saved", nil
}

// pasteForm returns the single form a provider's paste driver shows, for
// rendering on the page.
func (h *Handler) pasteForm(providerID string) browserlogin.Form {
	switch providerID {
	case "lidl":
		return browserlogin.LidlPaste().Form()
	case "action":
		return browserlogin.ActionPaste().Form()
	case "allegro":
		return browserlogin.AllegroPaste().Form()
	case "mail":
		return mailPaste().(*pasteFormDriver).form
	default:
		return browserlogin.Form{}
	}
}

func (h *Handler) cards(ctx context.Context) []providerCard {
	loggedIn := h.loggedInSet(ctx)
	cards := []providerCard{
		{ID: "lidl", Name: "Lidl Plus", Guided: true},
		{ID: "action", Name: "Action"},
		{ID: "allegro", Name: "Allegro"},
		{ID: "mail", Name: "Mailbox (e-mail fallback)"},
	}
	for i := range cards {
		cards[i].LoggedIn = loggedIn[cards[i].ID]
		cards[i].PasteForm = h.pasteForm(cards[i].ID)
	}
	return cards
}

// loggedInSet reads the backend status report and returns which providers are
// logged in. It tolerates the report being any JSON-serialisable shape.
func (h *Handler) loggedInSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	report, err := h.backend.StatusReport(ctx)
	if err != nil {
		return out
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return out
	}
	var parsed struct {
		Providers []struct {
			Provider string `json:"provider"`
			LoggedIn bool   `json:"logged_in"`
		} `json:"providers"`
		Mailbox struct {
			Configured bool `json:"configured"`
		} `json:"mailbox"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out
	}
	for _, p := range parsed.Providers {
		out[p.Provider] = p.LoggedIn
	}
	out["mail"] = parsed.Mailbox.Configured
	return out
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, r.URL.Query().Get("msg"))
}

func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, message string) {
	data := struct {
		Prefix  string
		Message string
		Cards   []providerCard
	}{
		Prefix:  h.prefix,
		Message: message,
		Cards:   h.cards(r.Context()),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func urlEscape(s string) string { return url.QueryEscape(s) }

var pageTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect your stores</title>
<style>
 :root{color-scheme:light dark}
 body{font:15px/1.5 system-ui,sans-serif;margin:0;background:#f5f5f4;color:#1c1917}
 @media(prefers-color-scheme:dark){body{background:#1c1917;color:#e7e5e4}.card{background:#292524!important;border-color:#44403c!important}}
 main{max-width:640px;margin:0 auto;padding:24px 16px 64px}
 h1{font-size:20px} .sub{color:#78716c;margin-top:-8px}
 .card{background:#fff;border:1px solid #e7e5e4;border-radius:12px;padding:16px;margin:16px 0}
 .row{display:flex;align-items:center;justify-content:space-between;gap:8px}
 .name{font-weight:600;font-size:16px}
 .pill{font-size:12px;padding:2px 8px;border-radius:999px;background:#e7e5e4;color:#44403c}
 .pill.on{background:#16a34a;color:#fff}
 label{display:block;margin:8px 0 2px;font-size:13px;color:#57534e}
 input{width:100%;box-sizing:border-box;padding:8px;border:1px solid #d6d3d1;border-radius:8px;background:transparent;color:inherit}
 button{margin-top:10px;padding:8px 14px;border:0;border-radius:8px;background:#2563eb;color:#fff;font-weight:600;cursor:pointer}
 button.ghost{background:#e7e5e4;color:#1c1917}
 .note{font-size:13px;color:#78716c;margin:4px 0}
 details summary{cursor:pointer;color:#2563eb;margin-top:8px}
 .msg{background:#dbeafe;color:#1e3a8a;padding:10px 12px;border-radius:8px;margin:12px 0}
 #guided .g-note{font-size:13px;color:#78716c}
</style></head><body><main>
<h1>Connect your stores</h1>
<p class="sub">Log in once per store; the token is stored encrypted and used by the receipts tools.</p>
{{if .Message}}<div class="msg">{{.Message}}</div>{{end}}
{{$prefix := .Prefix}}
{{range .Cards}}
<div class="card">
 <div class="row"><span class="name">{{.Name}}</span>
  <span class="pill {{if .LoggedIn}}on{{end}}">{{if .LoggedIn}}connected{{else}}not connected{{end}}</span></div>
 {{if .Guided}}
 <p class="note">Guided login: enter your details and we complete the store login for you (a code is texted to you).</p>
 <button class="g-btn" data-provider="{{.ID}}">Guided login</button>
 {{end}}
 <details{{if not .Guided}} open{{end}}><summary>{{if .Guided}}Or paste a token{{else}}Enter credentials{{end}}</summary>
  <form method="post" action="{{$prefix}}/paste">
   <input type="hidden" name="provider" value="{{.ID}}">
   {{if .PasteForm.Note}}<p class="note">{{.PasteForm.Note}}</p>{{end}}
   {{range .PasteForm.Fields}}
    <label>{{.Label}}</label>
    <input name="{{.Name}}" type="{{.Type}}" placeholder="{{.Placeholder}}" {{if .Required}}required{{end}}>
   {{end}}
   <button type="submit">Save</button>
  </form>
 </details>
</div>
{{end}}

<div id="guided" class="card" hidden>
 <div class="row"><span class="name" id="g-title">Guided login</span>
  <button class="ghost" id="g-cancel">Cancel</button></div>
 <p class="g-note" id="g-note"></p>
 <form id="g-form"></form>
 <div class="msg" id="g-msg" hidden></div>
</div>

<script>
const prefix={{$prefix}};
let session=null, provider=null;
const box=document.getElementById('guided'), form=document.getElementById('g-form'),
      note=document.getElementById('g-note'), title=document.getElementById('g-title'),
      msg=document.getElementById('g-msg');
function render(step){
  title.textContent=step.title||'Guided login';
  note.textContent=step.note||'';
  form.innerHTML='';
  (step.fields||[]).forEach(f=>{
    const l=document.createElement('label');l.textContent=f.label;form.appendChild(l);
    const i=document.createElement('input');i.name=f.name;i.type=f.type||'text';
    i.placeholder=f.placeholder||'';if(f.required)i.required=true;form.appendChild(i);
  });
  const b=document.createElement('button');b.type='submit';b.textContent='Continue';form.appendChild(b);
}
async function post(url,body){const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});return r.json();}
document.querySelectorAll('.g-btn').forEach(btn=>btn.onclick=async()=>{
  provider=btn.dataset.provider;box.hidden=false;msg.hidden=true;box.scrollIntoView();
  const res=await post(prefix+'/start',{provider});handle(res);
});
form.onsubmit=async(e)=>{e.preventDefault();
  const values={};new FormData(form).forEach((v,k)=>values[k]=v);
  const res=await post(prefix+'/step',{session,values});handle(res);
};
document.getElementById('g-cancel').onclick=async()=>{if(session)await post(prefix+'/cancel',{session});box.hidden=true;session=null;};
function handle(res){
  if(res.error){msg.hidden=false;msg.textContent=res.error;return;}
  if(res.done){msg.hidden=false;msg.textContent=res.message||'Connected.';form.innerHTML='';setTimeout(()=>location=prefix,1200);return;}
  session=res.session;render(res.step);
}
</script>
</main></body></html>`))
