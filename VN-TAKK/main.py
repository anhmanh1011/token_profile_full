import argparse
import json
import logging
import os
import queue
import re
import sys
import threading
import time
from datetime import datetime
from typing import Optional

from colorama import Fore, Style
from curl_cffi import requests

"""@jonnyssk"""
logging.basicConfig(
    level=logging.INFO,
    format=f"{Fore.CYAN}%(asctime)s{Style.RESET_ALL} - {Fore.WHITE}%(levelname)s{Style.RESET_ALL} - {Fore.GREEN}%(message)s{Style.RESET_ALL}",
    datefmt="%Y-%m-%d %H:%M:%S"
)
logger = logging.getLogger(__name__)

FOLDERCRETE = ''

_write_lock = threading.Lock()


def safe_write(path: str, line: str) -> None:
    with _write_lock:
        with open(path, 'a', encoding='utf-8') as fh:
            fh.write(line)


def create_timestamped_folder(base_name="result"):
    now = datetime.now()
    folder_name = f"{base_name}_{now.strftime('%d-%m-%y_%H-%M-%S')}"
    os.makedirs(folder_name, exist_ok=True)
    logger.info(f"Folder created: {folder_name}")
    return folder_name

def print_red_centered_art():
    print("\033[2J\033[H", end="")

    art = r'''

        _______                         _______    _
    |__   __|                       |__   __|  | |
        | | ___  __ _ _ __ ___  ___     | | ___ | | _____ _ __
        | |/ _ \/ _` | '_ ` _ \/ __|    | |/ _ \| |/ / _ \ '_ \
        | |  __/ (_| | | | | | \__ \    | | (_) |   <  __/ | | |
        |_|\___|\__,_|_| |_| |_|___/    |_|\___/|_|\_\___|_| |_|


    '''
    red_art = f"{Fore.RED}{art}{Style.RESET_ALL}"  # Set the text color to red
    logger.info(red_art.center(200))  # Adjust the width (80 characters) to match your terminal size



class TeamOutLook:

    def __init__(self, account: str, proxy: Optional[str] = None, redis_client: Optional['TokenRedis'] = None) -> None:
        self.mail, self.pwd = account.split(":")
        self.newpwd = self.pwd + "1"
        self.session = requests.Session(impersonate="firefox135",timeout=60)
        self.data_proxy = None
        self.tenant_id = ""
        self.redis_client = redis_client
        if proxy:
            if len(proxy.split(":")) == 2:

                    self.proxyies = proxy

            else:
                self.proxyies = proxy.split(":")[2] + ":" + proxy.split(":")[3] + "@" + proxy.split(":")[0] + ":" +proxy.split(":")[1]

            self.data_proxy  =  {
                        'http': f'http://{self.proxyies}',
                        'https': f'http://{self.proxyies}'
                    }

            self.session.proxies.update(self.data_proxy)

    def clear_cookies(self):
        self.session = requests.Session(impersonate='chrome131',timeout=60)

        if self.data_proxy:
            self.session.proxies.update(self.data_proxy)



    def authorize_common(self) -> 'str | bool':

        try:

            """Initinal url"""
            headers = {
                'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7',
                'accept-language': 'vi-VN,vi;q=0.9',
                'priority': 'u=0, i',
                'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                'sec-ch-ua-mobile': '?0',
                'sec-ch-ua-platform': '"Windows"',
                'sec-fetch-dest': 'document',
                'sec-fetch-mode': 'navigate',
                'sec-fetch-site': 'none',
                'sec-fetch-user': '?1',
                'upgrade-insecure-requests': '1',
                'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
            }

            params = {
                'client_id': '5e3ce6c0-2b1f-4285-8d4b-75ee78787346',
                'scope': 'openId profile openid offline_access',
                'redirect_uri': 'https://teams.microsoft.com/v2',
                'client-request-id': '019a2b63-12e6-751c-9549-6d43e8b3f31a',
                'response_mode': 'fragment',
                'response_type': 'code',
                'x-client-SKU': 'msal.js.browser',
                'x-client-VER': '3.30.0',
                'client_info': '1',
                'code_challenge': 'Io4JEa0ouaqyhy23EhsWfW8ukRmBhwyx-Lx9YOUvlII',
                'code_challenge_method': 'S256',
                'nonce': '019a2b63-12e6-7bdd-9e0c-ffd0d81dfa23',
                'state': 'eyJpZCI6IjAxOWEyYjYzLTEyZTYtNzA1Ni04NWQ4LTllNmQ5YTc2OGNiMCIsIm1ldGEiOnsiaW50ZXJhY3Rpb25UeXBlIjoicmVkaXJlY3QifX0=|https://teams.microsoft.com/v2/?culture=vi-vn&country=vn&enablemcasfort21=true',
                'sso_reload': 'true',
            }

            response = self.session.get('https://login.microsoftonline.com/common/oauth2/v2.0/authorize', params=params, headers=headers)

            return response.text

        except Exception as e:
            logger.info(f'{self.mail}     ------      Error Authorize {str(e)}')
            return False

    def login_common(self, response: str) -> 'str | bool':
        try:
            sFT = re.findall(r'"sFT":"(.*?)"',response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"',response)[0]
            canary = bytes(re.findall(r'"canary":"(.*?)"',response)[0],'utf-8').decode('unicode-escape')
        except (IndexError, KeyError) as exc:
            logger.warning(f'{self.mail} -- missing param: {exc}')
            return False

        headers = {
            'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7',
            'accept-language': 'vi-VN,vi;q=0.9',
            'cache-control': 'max-age=0',
            'content-type': 'application/x-www-form-urlencoded',
            'origin': 'https://login.microsoftonline.com',
            'priority': 'u=0, i',
            'referer': 'https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=5e3ce6c0-2b1f-4285-8d4b-75ee78787346&scope=openId%20profile%20openid%20offline_access&redirect_uri=https%3A%2F%2Fteams.microsoft.com%2Fv2&client-request-id=019a2b63-12e6-751c-9549-6d43e8b3f31a&response_mode=fragment&response_type=code&x-client-SKU=msal.js.browser&x-client-VER=3.30.0&client_info=1&code_challenge=D4kIaiwH89KuodNjLbswl4azyJfVvYFJjLITCkcW0Cc&code_challenge_method=S256&nonce=019a2b63-12e6-7bdd-9e0c-ffd0d81dfa23&state=eyJpZCI6IjAxOWEyYjYzLTEyZTYtNzA1Ni04NWQ4LTllNmQ5YTc2OGNiMCIsIm1ldGEiOnsiaW50ZXJhY3Rpb25UeXBlIjoicmVkaXJlY3QifX0%3D%7Chttps%3A%2F%2Fteams.microsoft.com%2Fv2%2F%3Fculture%3Dvi-vn%26country%3Dvn%26enablemcasfort21%3Dtrue&sso_reload=true',
            'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            'sec-ch-ua-mobile': '?0',
            'sec-ch-ua-platform': '"Windows"',
            'sec-fetch-dest': 'document',
            'sec-fetch-mode': 'navigate',
            'sec-fetch-site': 'same-origin',
            'sec-fetch-user': '?1',
            'upgrade-insecure-requests': '1',
            'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
        }

        data = {
            'i13': '0',
            'login': self.mail,
            'loginfmt': self.mail,
            'type': '11',
            'LoginOptions': '3',
            'lrt': '',
            'lrtPartition': '',
            'hisRegion': '',
            'hisScaleUnit': '',
            'passwd': self.pwd,
            'ps': '2',
            'psRNGCDefaultType': '',
            'psRNGCEntropy': '',
            'psRNGCSLK': '',
            'canary': canary,
            'ctx': sCtx,
            'hpgrequestid': 'de0904a5-1b6a-423c-9fcc-4f52f15e8700',
            'flowToken': sFT,
            'PPSX': '',
            'NewUser': '1',
            'FoundMSAs': '',
            'fspost': '0',
            'i21': '0',
            'CookieDisclosure': '0',
            'IsFidoSupported': '1',
            'isSignupPost': '0',
            'DfpArtifact': '',
            'i19': '20095',
        }



        response = self.session.post('https://login.microsoftonline.com/common/login?sso_reload=true',  headers=headers, data=data, allow_redirects=False)

        reporting = response.headers.get('reporting-endpoints', '')
        matches = re.findall(r'tenant=(.*?)"', reporting)
        if not matches:
            logger.warning(f'{self.mail} -- tenant not found in response')
            return False
        self.tenant_id = matches[0]

        return response.text

    def converged_change_password(self,response):

        logger.info(f'{self.mail}     ------      Password Changing Password')
        try:
            sFT = re.findall(r'"sFT":"(.*?)"',response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"',response)[0]
            canary = bytes(re.findall(r'"canary":"(.*?)"',response)[0],'utf-8').decode('unicode-escape')
        except (IndexError, KeyError) as exc:
            logger.warning(f'{self.mail} -- missing param: {exc}')
            return False

        try:

            headers = {
                    'accept': 'application/json',
                    'accept-language': 'vi-VN,vi;q=0.9',
                    'client-request-id': '019a2b63-12e6-751c-9549-6d43e8b3f31a',
                    'content-type': 'application/json; charset=UTF-8',
                    'hpgact': '2000',
                    'hpgid': '1116',
                    'hpgrequestid': 'e9537b48-02b0-43cf-a6c4-9c2fe1650800',
                    'origin': 'https://login.microsoftonline.com',
                    'priority': 'u=1, i',
                    'referer': 'https://login.microsoftonline.com/common/login?sso_reload=true',
                    'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                    'sec-ch-ua-mobile': '?0',
                    'sec-ch-ua-platform': '"Windows"',
                    'sec-fetch-dest': 'empty',
                    'sec-fetch-mode': 'cors',
                    'sec-fetch-site': 'same-origin',
                    'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
                    # 'cookie': 'esctx-LzXEtCyti3A=AQABCQEAAABlMNzVhAPUTrARzfQjWPtKsBrhSy6hRwMCGT5jwNh3MdGuSKLNZYNzhHhZj8kFH_hqL78QHHIREwmOGDtk8lysecwavMqqeKKVbtkxkwxANddZ8Ql5DHpP1Li3DajYfEe_2SNKJ35sTZ8X9Hkf1puDmNzgAb7u-mCO8x9WrbtvXiAA; x-ms-gateway-slice=estsfd; stsservicecookie=estsfd; MicrosoftApplicationsTelemetryDeviceId=75e29943-9cfd-494e-b99a-4aa7d5fc4f61; brcap=0; MSFPC=GUID=894b03100db64d2e8c3c7c03ad4d03be&HASH=894b&LV=202510&V=4&LU=1761664547948; wlidperf=FR=L&ST=1761664568797; esctx-ix9HLt4d2yA=AQABCQEAAABlMNzVhAPUTrARzfQjWPtKbXTdtRqY664Ek2swR2WWjpde2i2tPZqSdB3iKyl1NVo9OJEfXlSCPncdFVLq5nC9PjJDYcDYgzRl82aS4lIQ7DzYP_8qCvUmdQ4VTV40GtbB_Y5A4apGT5dl6kEIjiOAHfmWPkxW9GbRPVh8OEEG5CAA; AADSSO=NA|NoExtension; SSOCOOKIEPULLED=1; buid=1.AXEAMe_N-B6jSkuT5F9XHpElWsDmPF4fK4VCjUt17nh4c0YBAABxAA.AQABGgEAAABlMNzVhAPUTrARzfQjWPtK2H9latS_s9VkCsmUnNe0i3_LXbwfhmgubP7RQQIsRAuBmUouGDoqgxasvmg4EYqiCd8ELtgTNlVGVN6sxsQBhU2i2YFL78XfYYBFMA2UAwAgAA; esctx=PAQABBwEAAABlMNzVhAPUTrARzfQjWPtKCQFaJDODmllhBJn2S6dzylZAUwtJpM71hu-t2DcoceWXYtZN8JlNOhHVvNnoRbUZrBZIDLTyr8cnKB6H5w5BctGsHck_AQ23ThKW5Jz5f73HaG_U2SHq7zAcLHnnLphVHODloyD65-pGJoOLDNYfKbcvd1F2JTA25es-cA2i35MgAA; esctx-Wy2vLQb0LY=AQABCQEAAABlMNzVhAPUTrARzfQjWPtKWb2MBNCcvWkcwDFDI5ZagQQNujosn3M6RUhZSFCy-Mw6E7e8XiOL76H1qaPbHWBzy1my-jv0JACTCjjtBlmtVaii--5GePNg39GBVnKK9OxU8lvOZKr-QgoMnBTjZxvv5burStIcZqYYoSdAXG-05SAA; fpc=Ak3dFu9Hi51NpgmQrFsnpPUEezN4AQAAAB_VkuAOAAAAgDw67AEAAAAz1ZLgDgAAAA; ai_session=RdT6+TTmd5CZjNniJ4Y+91|1761664549376|1761664583099',
                }

            json_data = {
                'FlowToken': sFT,
                'Ctx': sCtx,
                'OldPassword': self.pwd,
                'NewPassword': self.newpwd,
            }


            response = self.session.post('https://login.microsoftonline.com/common/SSPR/Begin', headers=headers, json=json_data)
            sFT = response.json()['FlowToken']
            sCtx = response.json()['Ctx']




            """Poll"""
            headers = {
                'accept': 'application/json',
                'accept-language': 'vi-VN,vi;q=0.9',
                'client-request-id': '019a2b63-12e6-751c-9549-6d43e8b3f31a',
                'content-type': 'application/json; charset=UTF-8',
                'hpgact': '2000',
                'hpgid': '1116',
                'hpgrequestid': 'e9537b48-02b0-43cf-a6c4-9c2fe1650800',
                'origin': 'https://login.microsoftonline.com',
                'priority': 'u=1, i',
                'referer': 'https://login.microsoftonline.com/common/login?sso_reload=true',
                'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                'sec-ch-ua-mobile': '?0',
                'sec-ch-ua-platform': '"Windows"',
                'sec-fetch-dest': 'empty',
                'sec-fetch-mode': 'cors',
                'sec-fetch-site': 'same-origin',
                'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
            }

            json_data = {
                'FlowToken': sFT,
                'Ctx': sCtx,
                'CoupledDataCenter': 'PS1P',
                'CoupledScaleUnit': 'a',
            }

            response = self.session.post('https://login.microsoftonline.com/common/SSPR/Poll',  headers=headers, json=json_data)
            sFT = response.json()['FlowToken']
            sCtx = response.json()['Ctx']


            """End"""

            headers = {
                'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7',
                'accept-language': 'vi-VN,vi;q=0.9',
                'cache-control': 'max-age=0',
                'content-type': 'application/x-www-form-urlencoded',
                'origin': 'https://login.microsoftonline.com',
                'priority': 'u=0, i',
                'referer': 'https://login.microsoftonline.com/common/login?sso_reload=true',
                'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                'sec-ch-ua-mobile': '?0',
                'sec-ch-ua-platform': '"Windows"',
                'sec-fetch-dest': 'document',
                'sec-fetch-mode': 'navigate',
                'sec-fetch-site': 'same-origin',
                'sec-fetch-user': '?1',
                'upgrade-insecure-requests': '1',
                'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
            }

            data = {
                'ctx': sCtx,
                'hpgrequestid': 'e9537b48-02b0-43cf-a6c4-9c2fe1650800',
                'flowToken': sFT,
                'currentpasswd': self.pwd,
                'newpasswd': self.newpwd,
                'confirmnewpasswd': self.newpwd,
                'canary': canary,
                'i19': '24498',
            }

            response = self.session.post('https://login.microsoftonline.com/common/SSPR/End', headers=headers, data=data)

            self.pwd = self.newpwd
            safe_write(f'{FOLDERCRETE}/password_changed.txt', f'{self.mail}|{self.pwd}\n')
            logger.info(f'{self.mail}     ------      Password Changed')
            time.sleep(5)



        except Exception as e:
            logger.info(f'{self.mail}     ------      ERROR CHANGE PASSWORD {str(e)}')
            return False

        return True



    def login_kmsi(self,response):


        try:


            sFT = re.findall(r'"sFT":"(.*?)"',response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"',response)[0]
            canary = bytes(re.findall(r'"canary":"(.*?)"',response)[0],'utf-8').decode('unicode-escape')
        except (IndexError, KeyError) as exc:
            logger.warning(f'{self.mail} -- missing param: {exc}')
            logger.info(response)
            return False
        headers = {
            'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7',
            'accept-language': 'vi-VN,vi;q=0.9',
            'cache-control': 'max-age=0',
            'content-type': 'application/x-www-form-urlencoded',
            'origin': 'https://login.microsoftonline.com',
            'Host':'login.microsoftonline.com',
            'Referer':'https://login.microsoftonline.com/common/login?sso_reload=true',
            'priority': 'u=0, i',
            'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            'sec-ch-ua-mobile': '?0',
            'sec-ch-ua-platform': '"Windows"',
            'sec-fetch-dest': 'document',
            'sec-fetch-mode': 'navigate',
            'sec-fetch-site': 'same-origin',
            'sec-fetch-user': '?1',
            'upgrade-insecure-requests': '1',
            'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
        }

        data = {
            'LoginOptions': '1',
            'type': '28',
            'ctx': sCtx,
            'hpgrequestid': '61ae3a18-a16a-43a9-8d7d-88a726a32700',
            'flowToken': sFT,
            'canary': canary,
            'i19': '1720006',
        }

        response = self.session.post('https://login.microsoftonline.com/kmsi', headers=headers, data=data)

        try:
            data = json.loads(re.findall(r'"oPostParams":(.*?),"iMaxStackForKnockoutAsyncComponents"',response.text)[0])
            urlPost = re.findall(r'"urlPost":"(.*?)"',response.text)[0].replace('\\u0026',"&")
        except (IndexError, KeyError) as exc:
            logger.warning(f'{self.mail} -- missing param: {exc}')
            return False

        response = self.session.post('https://login.microsoftonline.com' + urlPost, headers=headers, data=data,allow_redirects=False)
        if "Object moved" not in response.text:
            logger.info(f'{self.mail}     ------      ERROR found next_localtion -- Maybe Wrong Password!!!!!!')
            return False
        try:
            code = re.findall(r'code=(.*?)&',response.headers.get('location'))[0]
        except (IndexError, KeyError) as exc:
            logger.warning(f'{self.mail} -- missing param: {exc}')
            return False

        return code

    def GetAccessToken(self,code):
        headers = {
            'accept': '*/*',
            'accept-language': 'vi-VN,vi;q=0.9',
            'content-type': 'application/x-www-form-urlencoded;charset=utf-8',
            'origin': 'https://teams.microsoft.com',
            'priority': 'u=1, i',
            'referer': 'https://teams.microsoft.com/',
            'sec-ch-ua': '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            'sec-ch-ua-mobile': '?0',
            'sec-ch-ua-platform': '"Windows"',
            'sec-fetch-dest': 'empty',
            'sec-fetch-mode': 'cors',
            'sec-fetch-site': 'cross-site',
            'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
        }

        params = {
            'client-request-id': '019a9d34-4a19-734e-b5de-edf940f3dd53',
        }

        data = f'client_id=5e3ce6c0-2b1f-4285-8d4b-75ee78787346&redirect_uri=https%3A%2F%2Fteams.microsoft.com%2Fv2&scope=openId%20profile%20openid%20offline_access&code={code}&x-client-SKU=msal.js.browser&x-client-VER=3.30.0&x-ms-lib-capability=retry-after, h429&x-client-current-telemetry=5|865,0,,,|,&x-client-last-telemetry=5|0|863,Core-2e7cf48a-7c96-48cc-a60a-db7619709e55|login_required|1,0&code_verifier=_jGDmd391UKSoXF8aeTSAEPmlmTpPuFM_eRuv0q_V9Y&grant_type=authorization_code&client_info=1&X-AnchorMailbox=Oid%3A106ad7cc-d337-4db3-a450-qweqweqwewqewqeqweqwe%401b22c983-1cd9-4e21-9dab-be096b10f075'

        response = self.session.post('https://login.microsoftonline.com/common/oauth2/v2.0/token', params=params, headers=headers, data=data)

        if 'refresh_token' not in response.text:
            logger.info(f'{self.mail}     ------      ERROR not found refresh_token')
            return False
        rf = response.json()['refresh_token']


        params = {
            'client-request-id': 'ba46b71e-fb4f-4a0d-bed8-4834081766dd',
        }

        data = F'client_id=5e3ce6c0-2b1f-4285-8d4b-75ee78787346&redirect_uri=https%3A%2F%2Fteams.microsoft.com%2Fv2%2Fauth&scope=https%3A%2F%2Floki.delve.office.com%2F%2F.default%20openid%20profile%20offline_access&grant_type=refresh_token&client_info=1&x-client-SKU=msal.js.browser&x-client-VER=3.30.0&x-ms-lib-capability=retry-after, h429&x-client-current-telemetry=5|61,0,,,|,&x-client-last-telemetry=5|0|||0,0&refresh_token={rf}&X-AnchorMailbox=Oid%3A4462d551-dfc1-459c-aa08-a2335ewqeqweqwede6ef2d%401b22c983-1cd9-4e21-9dab-be096b10f075'

        response = self.session.post(
            f'https://login.microsoftonline.com/{self.tenant_id}/oauth2/v2.0/token',
            params=params,
            headers=headers,
            data=data,

        )



        if 'access_token' not in response.text:
            logger.info(f'{self.mail}     ------      ERROR not found access_token')
            return False

        if self.redis_client:
            self.redis_client.push_token(self.mail, self.pwd, rf, self.tenant_id)
        else:
            safe_write(f'{FOLDERCRETE}/get_token_success.txt',
                       f'{self.mail}:{self.pwd}|{rf}|{self.tenant_id}\n')
        logger.info(f'{self.mail}     ------      Get token success - Tenant: {self.tenant_id}')
        return True

    def DoTask(self) -> bool:
        try:
            res__authorize_common = self.authorize_common()

            if not res__authorize_common:
                return False
            res_login_common = self.login_common(res__authorize_common)
            if not res_login_common:
                return False
            if "ConvergedChangePassword" in res_login_common:
                res_conver = self.converged_change_password(res_login_common)
                if not res_conver:
                    return False

            self.clear_cookies()

            res__authorize_common = self.authorize_common()
            if not res__authorize_common:
                return False
            res_login_common = self.login_common(res__authorize_common)
            if not res_login_common:
                return False
            code_auth = self.login_kmsi(res_login_common)
            if not code_auth:
                return False
            res_access_token = self.GetAccessToken(code_auth)
            if not res_access_token:
                return False
            return True

        except Exception as e:
            logger.info(f'{self.mail}     ------      ERROR {str(e)}')
            return False


def main():
    global FOLDERCRETE

    parser = argparse.ArgumentParser()
    parser.add_argument('--redis', default=None, help='Redis host:port (e.g. 127.0.0.1:6379)')
    parser.add_argument('--tenant', default=None, help='Tenant ID to process')
    parser.add_argument('--accounts', default='account.txt', help='Accounts file')
    parser.add_argument('--threads', type=int, default=100, help='Number of threads')
    args = parser.parse_args()

    print_red_centered_art()
    start_time = time.time()
    FOLDERCRETE = create_timestamped_folder()

    redis_client = None
    if args.redis:
        from redis_client import TokenRedis
        host, port = args.redis.split(':')
        redis_client = TokenRedis(host=host, port=int(port))

    with open(args.accounts, 'r', encoding='utf-8') as f:
        listmail = f.read().splitlines()

    if redis_client and args.tenant:
        listmail = redis_client.get_accounts_to_login(args.tenant, listmail)
        logger.info(f"Filtered to {len(listmail)} accounts needing login")

    if not listmail:
        logger.info("No accounts to process")
        return

    task_queue = queue.Queue()
    for mail in listmail:
        task_queue.put(mail)

    stop_event = threading.Event()
    hits = 0
    fails = 0
    lock = threading.Lock()

    def worker(thread_id):
        nonlocal hits, fails
        while not stop_event.is_set():
            try:
                mail = task_queue.get(timeout=1)
            except queue.Empty:
                return

            try:
                obj = TeamOutLook(mail, redis_client=redis_client)
                if obj.DoTask():
                    with lock:
                        hits += 1
                else:
                    safe_write(f'{FOLDERCRETE}/failed_get.txt', f'{mail}\n')
                    with lock:
                        fails += 1
            except Exception as e:
                logger.error(f"[ERROR] {mail}: {e}")
                with lock:
                    fails += 1
            finally:
                task_queue.task_done()

    threads = []
    for i in range(args.threads):
        t = threading.Thread(target=worker, args=(i,), daemon=True)
        threads.append(t)
        t.start()

    task_queue.join()

    end_time = time.time()
    logger.info(f"Completed {len(listmail)} accounts in {(end_time - start_time)/60:.2f} min | Hits: {hits} | Fails: {fails}")

    if redis_client and args.tenant:
        stats = redis_client.stats(args.tenant)
        logger.info(f"Redis stats: {stats}")

if __name__ == "__main__":
    main()
