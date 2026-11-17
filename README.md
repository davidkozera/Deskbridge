# Deskbridge

A remote desktop tool for Windows PCs, including Windows on Arm: screen, keyboard, mouse, sound, copy and paste, and files, with a server that runs at startup and works at the lock screen.

Everything about installing and using it is in [README.txt](README.txt). Setup programs are on the [Releases](https://github.com/davidkozera/Deskbridge/releases) page.

## Internet relay (Alpha)

Alpha versions can reach a PC on another network through a relay, with no ports opened on any router. The relay is one small container, and this button creates it on Azure Container Apps in your own subscription:

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2Fdavidkozera%2FDeskbridge%2Falpha%2Fazuredeploy.json)

1. Click the button and sign in to Azure.
2. Choose a subscription, create a resource group (for example `deskbridge`) and pick a region. Leave the other fields alone.
3. Click **Review + create**, then **Create**, and wait a minute or two.
4. On the finished deployment, open **Outputs** and copy **relayCode**. It looks like `https://name.region.azurecontainerapps.io#key`.
5. In the Deskbridge client, choose **More > Internet access: use the relay code I copied**, then connect once to each saved PC while on its own network.

The relay code holds the relay's key, so treat it like a password. It can be read again later under the deployment's Outputs, or pieced together from the container app's address and its `relay-key` secret.

What the relay does, what it costs to run and what it cannot see are described under INTERNET in [README.txt](README.txt). The template is [azuredeploy.json](azuredeploy.json).
