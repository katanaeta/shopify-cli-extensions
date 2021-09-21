messages:
    extension_url: Your extension is available at {{ .Assets.Main.Url }}
{{ if eq (.Surface) ("Checkout") }}
    login: If this is first time you are testing the extension, please login to your development store first by visiting {{ .Store}}/password
redirect_url: https://{{ .Store }}/{{ .Development.Resource.Url }}?dev={{ .ApiUrl }}
{{ end }}
{{ if eq (.Surface) ("Admin") }}
    login: If this is first time you are testing the extension, please login to your development store Admin first by visiting {{ .Store}}/admin
redirect_url: https://{{ .Store }}/admin/extensions-dev?url={{ .ApiUrl }}&redirect={{ .Development.Root.Url }}
{{ end }}