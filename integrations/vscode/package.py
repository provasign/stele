#!/usr/bin/env python3
"""Build the dependency-free VS Code client as a local VSIX (does not publish)."""
import argparse
import json
from pathlib import Path
import xml.etree.ElementTree as ET
from xml.sax.saxutils import escape
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    package = json.loads((source / "package.json").read_text())
    manifest = f'''<?xml version="1.0" encoding="utf-8"?>
<PackageManifest Version="2.0.0" xmlns="http://schemas.microsoft.com/developer/vsx-schema/2011">
  <Metadata>
    <Identity Language="en-US" Id="{escape(package['name'])}" Version="{escape(package['version'])}" Publisher="{escape(package['publisher'])}"/>
    <DisplayName>{escape(package['displayName'])}</DisplayName>
    <Description xml:space="preserve">{escape(package['description'])}</Description>
    <Tags>stele,specifications,diagnostics</Tags><Categories>Linters</Categories><GalleryFlags>Preview</GalleryFlags>
    <Properties>
      <Property Id="Microsoft.VisualStudio.Code.Engine" Value="{escape(package['engines']['vscode'])}"/>
      <Property Id="Microsoft.VisualStudio.Code.ExtensionKind" Value="workspace"/>
    </Properties>
  </Metadata>
  <Installation><InstallationTarget Id="Microsoft.VisualStudio.Code"/></Installation><Dependencies/>
  <Assets><Asset Type="Microsoft.VisualStudio.Code.Manifest" Path="extension/package.json" Addressable="true"/></Assets>
</PackageManifest>'''
    types = '''<?xml version="1.0" encoding="utf-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="json" ContentType="application/json"/>
<Default Extension="js" ContentType="application/javascript"/>
<Default Extension="vsixmanifest" ContentType="text/xml"/>
<Override PartName="/extension/LICENSE" ContentType="text/plain"/>
</Types>'''
    ET.fromstring(manifest)
    ET.fromstring(types)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(args.out, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("extension.vsixmanifest", manifest)
        archive.writestr("[Content_Types].xml", types)
        for name in ("package.json", "extension.js"):
            archive.write(source / name, "extension/" + name)
        license_file = source.parents[1] / "LICENSE"
        if license_file.exists():
            archive.write(license_file, "extension/LICENSE")
    with zipfile.ZipFile(args.out) as archive:
        assert archive.testzip() is None
        assert json.loads(archive.read("extension/package.json"))["main"] == "./extension.js"
    print(args.out)


if __name__ == "__main__":
    main()
