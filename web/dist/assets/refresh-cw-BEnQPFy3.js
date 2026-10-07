import{$t as e,An as t,B as n,I as r,Jt as i,Ln as a,N as o,Q as s,Qt as c,Sn as l,Vn as u,Yt as d,Zt as f,_t as p,a as m,b as h,dt as g,en as _,et as v,fn as y,gn as b,gt as x,ht as S,it as C,kn as w,mn as T,n as E,nt as D,pn as O,pt as k,st as A,x as j,yn as M,zn as N}from"./index-rDtZlfsP.js";var P={closeIconSizeTiny:`12px`,closeIconSizeSmall:`12px`,closeIconSizeMedium:`14px`,closeIconSizeLarge:`14px`,closeSizeTiny:`16px`,closeSizeSmall:`16px`,closeSizeMedium:`18px`,closeSizeLarge:`18px`,padding:`0 7px`,closeMargin:`0 0 0 4px`};function F(e){let{textColor2:t,primaryColorHover:n,primaryColorPressed:r,primaryColor:i,infoColor:a,successColor:o,warningColor:s,errorColor:c,baseColor:l,borderColor:u,opacityDisabled:d,tagColor:f,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,borderRadiusSmall:g,fontSizeMini:_,fontSizeTiny:v,fontSizeSmall:y,fontSizeMedium:b,heightMini:x,heightTiny:S,heightSmall:w,heightMedium:T,closeColorHover:E,closeColorPressed:D,buttonColor2Hover:O,buttonColor2Pressed:k,fontWeightStrong:A}=e;return{...P,closeBorderRadius:g,heightTiny:x,heightSmall:S,heightMedium:w,heightLarge:T,borderRadius:g,opacityDisabled:d,fontSizeTiny:_,fontSizeSmall:v,fontSizeMedium:y,fontSizeLarge:b,fontWeightStrong:A,textColorCheckable:t,textColorHoverCheckable:t,textColorPressedCheckable:t,textColorChecked:l,colorCheckable:`#0000`,colorHoverCheckable:O,colorPressedCheckable:k,colorChecked:i,colorCheckedHover:n,colorCheckedPressed:r,border:`1px solid ${u}`,textColor:t,color:f,colorBordered:`rgb(250, 250, 252)`,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,closeColorHover:E,closeColorPressed:D,borderPrimary:`1px solid ${C(i,{alpha:.3})}`,textColorPrimary:i,colorPrimary:C(i,{alpha:.12}),colorBorderedPrimary:C(i,{alpha:.1}),closeIconColorPrimary:i,closeIconColorHoverPrimary:i,closeIconColorPressedPrimary:i,closeColorHoverPrimary:C(i,{alpha:.12}),closeColorPressedPrimary:C(i,{alpha:.18}),borderInfo:`1px solid ${C(a,{alpha:.3})}`,textColorInfo:a,colorInfo:C(a,{alpha:.12}),colorBorderedInfo:C(a,{alpha:.1}),closeIconColorInfo:a,closeIconColorHoverInfo:a,closeIconColorPressedInfo:a,closeColorHoverInfo:C(a,{alpha:.12}),closeColorPressedInfo:C(a,{alpha:.18}),borderSuccess:`1px solid ${C(o,{alpha:.3})}`,textColorSuccess:o,colorSuccess:C(o,{alpha:.12}),colorBorderedSuccess:C(o,{alpha:.1}),closeIconColorSuccess:o,closeIconColorHoverSuccess:o,closeIconColorPressedSuccess:o,closeColorHoverSuccess:C(o,{alpha:.12}),closeColorPressedSuccess:C(o,{alpha:.18}),borderWarning:`1px solid ${C(s,{alpha:.35})}`,textColorWarning:s,colorWarning:C(s,{alpha:.15}),colorBorderedWarning:C(s,{alpha:.12}),closeIconColorWarning:s,closeIconColorHoverWarning:s,closeIconColorPressedWarning:s,closeColorHoverWarning:C(s,{alpha:.12}),closeColorPressedWarning:C(s,{alpha:.18}),borderError:`1px solid ${C(c,{alpha:.23})}`,textColorError:c,colorError:C(c,{alpha:.1}),colorBorderedError:C(c,{alpha:.08}),closeIconColorError:c,closeIconColorHoverError:c,closeIconColorPressedError:c,closeColorHoverError:C(c,{alpha:.12}),closeColorPressedError:C(c,{alpha:.18})}}var I={name:`Tag`,common:D,self:F},L={color:Object,type:{type:String,default:`default`},round:Boolean,size:String,closable:Boolean,disabled:{type:Boolean,default:void 0}},R=d(`tag`,`
 --n-close-margin: var(--n-close-margin-top) var(--n-close-margin-right) var(--n-close-margin-bottom) var(--n-close-margin-left);
 white-space: nowrap;
 position: relative;
 box-sizing: border-box;
 cursor: default;
 display: inline-flex;
 align-items: center;
 flex-wrap: nowrap;
 padding: var(--n-padding);
 border-radius: var(--n-border-radius);
 color: var(--n-text-color);
 background-color: var(--n-color);
 transition: 
 border-color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier),
 opacity .3s var(--n-bezier);
 line-height: 1;
 height: var(--n-height);
 font-size: var(--n-font-size);
`,[c(`strong`,`
 font-weight: var(--n-font-weight-strong);
 `),f(`border`,`
 pointer-events: none;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 border-radius: inherit;
 border: var(--n-border);
 transition: border-color .3s var(--n-bezier);
 `),f(`icon`,`
 display: flex;
 margin: 0 4px 0 0;
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 font-size: var(--n-avatar-size-override);
 `),f(`avatar`,`
 display: flex;
 margin: 0 6px 0 0;
 `),f(`close`,`
 margin: var(--n-close-margin);
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),c(`round`,`
 padding: 0 calc(var(--n-height) / 3);
 border-radius: calc(var(--n-height) / 2);
 `,[f(`icon`,`
 margin: 0 4px 0 calc((var(--n-height) - 8px) / -2);
 `),f(`avatar`,`
 margin: 0 6px 0 calc((var(--n-height) - 8px) / -2);
 `),c(`closable`,`
 padding: 0 calc(var(--n-height) / 4) 0 calc(var(--n-height) / 3);
 `)]),c(`icon, avatar`,[c(`round`,`
 padding: 0 calc(var(--n-height) / 3) 0 calc(var(--n-height) / 2);
 `)]),c(`disabled`,`
 cursor: not-allowed !important;
 opacity: var(--n-opacity-disabled);
 `),c(`checkable`,`
 cursor: pointer;
 box-shadow: none;
 color: var(--n-text-color-checkable);
 background-color: var(--n-color-checkable);
 `,[e(`disabled`,[i(`&:hover`,`background-color: var(--n-color-hover-checkable);`,[e(`checked`,`color: var(--n-text-color-hover-checkable);`)]),i(`&:active`,`background-color: var(--n-color-pressed-checkable);`,[e(`checked`,`color: var(--n-text-color-pressed-checkable);`)])]),c(`checked`,`
 color: var(--n-text-color-checked);
 background-color: var(--n-color-checked);
 `,[e(`disabled`,[i(`&:hover`,`background-color: var(--n-color-checked-hover);`),i(`&:active`,`background-color: var(--n-color-checked-pressed);`)])])])]),z=[`onClick`,`onMouseenter`,`onMouseleave`],B={...s.props,...L,bordered:{type:Boolean,default:void 0},checked:Boolean,checkable:Boolean,strong:Boolean,triggerClickOnClose:Boolean,onClose:[Array,Function],onMouseenter:Function,onMouseleave:Function,"onUpdate:checked":Function,onUpdateChecked:Function,internalCloseFocusable:{type:Boolean,default:!0},internalCloseIsButtonTag:{type:Boolean,default:!0},onCheckedChange:Function},V=x(`n-tag`),H=M({name:`Tag`,props:B,slots:Object,setup(e){let r=a(null),{mergedBorderedRef:i,mergedClsPrefixRef:c,inlineThemeDisabled:l,mergedRtlRef:u,mergedComponentPropsRef:d}=S(e),f=y(()=>e.size||d?.value?.Tag?.size||`medium`),p=s(`Tag`,`-tag`,R,I,e,c);t(V,{roundRef:N(e,`round`)});function m(){if(!e.disabled&&e.checkable){let{checked:t,onCheckedChange:n,onUpdateChecked:r,"onUpdate:checked":i}=e;r&&r(!t),i&&i(!t),n&&n(!t)}}function h(t){if(e.triggerClickOnClose||t.stopPropagation(),!e.disabled){let{onClose:r}=e;r&&n(r,t)}}let g={setTextContent(e){let{value:t}=r;t&&(t.textContent=e)}},b=o(`Tag`,u,c),x=y(()=>{let{type:t,color:{color:n,textColor:r}={}}=e,a=f.value,{common:{cubicBezierEaseInOut:o},self:{padding:s,closeMargin:c,borderRadius:l,opacityDisabled:u,textColorCheckable:d,textColorHoverCheckable:m,textColorPressedCheckable:h,textColorChecked:g,colorCheckable:v,colorHoverCheckable:y,colorPressedCheckable:b,colorChecked:x,colorCheckedHover:S,colorCheckedPressed:C,closeBorderRadius:w,fontWeightStrong:T,[_(`colorBordered`,t)]:E,[_(`closeSize`,a)]:D,[_(`closeIconSize`,a)]:O,[_(`fontSize`,a)]:k,[_(`height`,a)]:j,[_(`color`,t)]:M,[_(`textColor`,t)]:N,[_(`border`,t)]:P,[_(`closeIconColor`,t)]:F,[_(`closeIconColorHover`,t)]:I,[_(`closeIconColorPressed`,t)]:L,[_(`closeColorHover`,t)]:R,[_(`closeColorPressed`,t)]:z}}=p.value,B=A(c);return{"--n-font-weight-strong":T,"--n-avatar-size-override":`calc(${j} - 8px)`,"--n-bezier":o,"--n-border-radius":l,"--n-border":P,"--n-close-icon-size":O,"--n-close-color-pressed":z,"--n-close-color-hover":R,"--n-close-border-radius":w,"--n-close-icon-color":F,"--n-close-icon-color-hover":I,"--n-close-icon-color-pressed":L,"--n-close-icon-color-disabled":F,"--n-close-margin-top":B.top,"--n-close-margin-right":B.right,"--n-close-margin-bottom":B.bottom,"--n-close-margin-left":B.left,"--n-close-size":D,"--n-color":n||(i.value?E:M),"--n-color-checkable":v,"--n-color-checked":x,"--n-color-checked-hover":S,"--n-color-checked-pressed":C,"--n-color-hover-checkable":y,"--n-color-pressed-checkable":b,"--n-font-size":k,"--n-height":j,"--n-opacity-disabled":u,"--n-padding":s,"--n-text-color":r||N,"--n-text-color-checkable":d,"--n-text-color-checked":g,"--n-text-color-hover-checkable":m,"--n-text-color-pressed-checkable":h}}),C=l?v(`tag`,y(()=>{let t=``,{type:n,color:{color:r,textColor:a}={}}=e;return t+=n[0],t+=f.value[0],r&&(t+=`a${j(r)}`),a&&(t+=`b${j(a)}`),i.value&&(t+=`c`),t}),x,e):void 0;return{...g,rtlEnabled:b,mergedClsPrefix:c,contentRef:r,mergedBordered:i,handleClick:m,handleCloseClick:h,cssVars:l?void 0:x,themeClass:C?.themeClass,onRender:C?.onRender}},render(){let{mergedClsPrefix:e,rtlEnabled:t,closable:n,color:{borderColor:i}={},round:a,onRender:o,$slots:s}=this;o?.();let c=r(s.avatar,t=>t&&(w(),b(`div`,{class:g(`${e}-tag__avatar`)},[k(()=>t)],2))),l=r(s.icon,t=>t&&(w(),b(`div`,{class:g(`${e}-tag__icon`)},[k(()=>t)],2)));return w(),b(`div`,{class:g([`${e}-tag`,this.themeClass,{[`${e}-tag--rtl`]:t,[`${e}-tag--strong`]:this.strong,[`${e}-tag--disabled`]:this.disabled,[`${e}-tag--checkable`]:this.checkable,[`${e}-tag--checked`]:this.checkable&&this.checked,[`${e}-tag--round`]:a,[`${e}-tag--avatar`]:c,[`${e}-tag--icon`]:l,[`${e}-tag--closable`]:n}]),style:u(this.cssVars),onClick:this.handleClick,onMouseenter:this.onMouseenter,onMouseleave:this.onMouseleave},[k(()=>l||c),O(`span`,{class:g(`${e}-tag__content`),ref:`contentRef`},[k(()=>this.$slots.default?.())],2),!this.checkable&&n?(w(),T(h,{key:0,clsPrefix:e,class:g(`${e}-tag__close`),disabled:this.disabled,onClick:this.handleCloseClick,focusable:this.internalCloseFocusable,round:a,isButtonTag:this.internalCloseIsButtonTag,absolute:!0},null,8,[`clsPrefix`,`class`,`disabled`,`onClick`,`focusable`,`round`,`isButtonTag`])):k(()=>null),!this.checkable&&this.mergedBordered?(w(),b(`div`,{key:2,class:g(`${e}-tag__border`),style:u({borderColor:i})},null,6)):k(()=>null)],46,z)}});function U(){let e=l(m,null);return e===null&&p(`use-dialog`,`No outer <n-dialog-provider /> founded.`),e}var W=E({name:`refresh-cw`,size:24,node:[[`path`,{d:`M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8`,key:`v9h5vc`}],[`path`,{d:`M21 3v5h-5`,key:`1q7to0`}],[`path`,{d:`M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16`,key:`3uifl3`}],[`path`,{d:`M8 16H3v5`,key:`1cv678`}]]});export{U as n,H as r,W as t};